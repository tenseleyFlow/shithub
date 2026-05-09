// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

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
//     + audit. We process inline rather than fanning out to one job
//     per repo because hard-deletes are rare and the per-row cost is
//     small.
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
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := lifecycle.HardDelete(ctx, ldeps, 0, id); err != nil {
				deps.Logger.WarnContext(ctx, "lifecycle:sweep: hard delete failed",
					"repo_id", id, "error", err)
				// Keep going — one bad row shouldn't poison the sweep.
			}
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
			deps.Logger.WarnContext(ctx, "lifecycle:sweep: list past-grace orgs", "error", err)
		}
		for _, oid := range orgIDs {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := orgs.HardDelete(ctx, ohd, oid); err != nil {
				deps.Logger.WarnContext(ctx, "lifecycle:sweep: org hard delete failed",
					"org_id", oid, "error", err)
			}
		}
		return nil
	}
}
