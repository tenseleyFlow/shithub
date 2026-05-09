// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// IndexReconcileDeps wires the reconciler. Same shape as the rest
// of the index-adjacent jobs.
type IndexReconcileDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// reconcileBatch is the per-tick cap on how many drifted repos the
// reconciler enqueues. Bounded so a single reconciler tick can't
// spike the queue depth on a fresh deploy where every repo's
// `last_indexed_oid` is NULL.
const reconcileBatch = 100

// RepoIndexReconcile compares `repos.default_branch_oid` against
// `repos.last_indexed_oid` and enqueues a `repo:index_code` job for
// each drifted row. Self-throttling: only `reconcileBatch` repos
// per tick.
//
// Designed to be invoked from a cron schedule (e.g. every 5 minutes)
// once the cron framework lands. For now we expose it as a worker
// job so an operator can invoke it manually via the admin enqueue
// path, and the regular worker pool runs it on enqueue.
func RepoIndexReconcile(deps IndexReconcileDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		repos, err := reposdb.New().ListReposNeedingReindex(ctx, deps.Pool, reconcileBatch)
		if err != nil {
			return err
		}
		for _, r := range repos {
			if _, err := worker.Enqueue(
				ctx, deps.Pool, worker.KindRepoIndexCode,
				map[string]any{"repo_id": r.ID},
				worker.EnqueueOptions{},
			); err != nil {
				if deps.Logger != nil {
					deps.Logger.WarnContext(ctx, "reconcile: enqueue failed",
						"repo_id", r.ID, "error", err)
				}
				// Don't fail the whole reconciler — keep enqueuing
				// the rest. A single transient enqueue failure
				// shouldn't poison the batch.
			}
		}
		if deps.Logger != nil && len(repos) > 0 {
			deps.Logger.InfoContext(ctx, "reconcile: enqueued reindex jobs", "count", len(repos))
		}
		return nil
	}
}
