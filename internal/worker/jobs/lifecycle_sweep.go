// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const lifecycleSweepHardDeleteTimeout = 45 * time.Second

// LifecycleSweepDeps wires the periodic sweep handler.
type LifecycleSweepDeps struct {
	Pool   *pgxpool.Pool
	RepoFS *storage.RepoFS
	Audit  *audit.Recorder
	Logger *slog.Logger
}

// LifecycleSweep runs two related housekeeping passes in one job:
//
//  1. Hard-delete every repo whose deleted_at is past the grace window.
//     Each repo gets the full lifecycle.HardDelete cascade — FS + DB
//     + audit. Each row gets a bounded child context so one stuck
//     tombstone cannot consume the entire worker job timeout and starve
//     later rows.
//  2. Flip pending transfer requests past expires_at to "expired".
//
// Enqueue this kind from a cron timer (S37 ships the systemd cron
// service); for now the operator can `INSERT` a job manually or call
// it once at boot.
func LifecycleSweep(deps LifecycleSweepDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		// 1. Hard-delete past-grace repos.
		rq := reposdb.New()
		ids, err := rq.ListRepoIDsPastSoftDeleteGrace(ctx, deps.Pool)
		if err != nil {
			return err
		}
		ldeps := lifecycle.Deps{
			Pool: deps.Pool, RepoFS: deps.RepoFS,
			Audit: deps.Audit, Logger: deps.Logger,
		}
		if err := runLifecycleSweepHardDeletes(ctx, ids, lifecycleSweepHardDeleteTimeout, deps.Logger, func(deleteCtx context.Context, id int64) error {
			return lifecycle.HardDelete(deleteCtx, ldeps, 0, id)
		}); err != nil {
			return err
		}

		// 2. Expire pending transfers past their TTL.
		n, err := lifecycle.ExpirePending(ctx, ldeps)
		if err != nil {
			return err
		}
		if n > 0 {
			deps.Logger.InfoContext(ctx, "lifecycle:sweep: expired transfers", "count", n)
		}

		// 3. S30 — hard-delete orgs past their 14-day grace window.
		// Cascade per-repo via lifecycle.HardDelete inside the org
		// orchestrator. Same per-row failure-tolerant shape as the
		// repo path above.
		odeps := orgs.Deps{Pool: deps.Pool, Logger: deps.Logger, Audit: deps.Audit}
		ohd := orgs.HardDeleteDeps{Deps: odeps, RepoFS: deps.RepoFS, Audit: deps.Audit}
		orgIDs, err := orgs.ListPastGraceOrgIDs(ctx, odeps)
		if err != nil {
			warnLifecycleSweep(ctx, deps.Logger, "lifecycle:sweep: list past-grace orgs", "error", err)
		}
		if err := runLifecycleSweepOrgHardDeletes(ctx, orgIDs, lifecycleSweepHardDeleteTimeout, deps.Logger, func(deleteCtx context.Context, id int64) error {
			return orgs.HardDelete(deleteCtx, ohd, id)
		}); err != nil {
			return err
		}
		return nil
	}
}

func runLifecycleSweepHardDeletes(
	ctx context.Context,
	ids []int64,
	timeout time.Duration,
	logger *slog.Logger,
	hardDelete func(context.Context, int64) error,
) error {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleteCtx, cancel := context.WithTimeout(ctx, timeout)
		err := hardDelete(deleteCtx, id)
		cancel()
		if err != nil {
			warnLifecycleSweep(ctx, logger, "lifecycle:sweep: hard delete failed",
				"repo_id", id, "error", err)
			// Keep going — one bad row shouldn't poison the sweep.
		}
	}
	return nil
}

func runLifecycleSweepOrgHardDeletes(
	ctx context.Context,
	ids []int64,
	timeout time.Duration,
	logger *slog.Logger,
	hardDelete func(context.Context, int64) error,
) error {
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		deleteCtx, cancel := context.WithTimeout(ctx, timeout)
		err := hardDelete(deleteCtx, id)
		cancel()
		if err != nil {
			warnLifecycleSweep(ctx, logger, "lifecycle:sweep: org hard delete failed",
				"org_id", id, "error", err)
		}
	}
	return nil
}

func warnLifecycleSweep(ctx context.Context, logger *slog.Logger, msg string, args ...any) {
	if logger != nil {
		logger.WarnContext(ctx, msg, args...)
	}
}
