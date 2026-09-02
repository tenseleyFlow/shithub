// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	repotraffic "github.com/tenseleyFlow/shithub/internal/repos/traffic"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// TrafficPurgeDeps wires the traffic retention sweep.
type TrafficPurgeDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// TrafficPurgePayload overrides the defaults for one run. An empty
// object (what cron sends) selects the package defaults; operators can
// pass a shorter window through `shithubd admin run-job` to drain a
// backlog faster.
type TrafficPurgePayload struct {
	RetentionDays      int `json:"retention_days,omitempty"`
	DailyRetentionDays int `json:"daily_retention_days,omitempty"`
	BatchSize          int `json:"batch_size,omitempty"`
	MaxBatches         int `json:"max_batches,omitempty"`
}

// TrafficPurge trims repo_traffic_uniques / _paths / _referrers to a
// 30-day window and repo_traffic_daily to 400 days, in bounded batches.
// Cron-driven and idempotent: the cutoff is recomputed from the clock
// on every run and rows inside the window are never touched, so a
// re-run after a partial pass simply picks up where it stopped.
func TrafficPurge(deps TrafficPurgeDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p TrafficPurgePayload
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				// A malformed payload will never parse on a retry.
				return worker.PoisonError(err)
			}
		}
		res, err := repotraffic.Purge(ctx, deps.Pool, repotraffic.PurgeOptions{
			RetentionDays:      p.RetentionDays,
			DailyRetentionDays: p.DailyRetentionDays,
			BatchSize:          p.BatchSize,
			MaxBatches:         p.MaxBatches,
		})
		if deps.Logger != nil {
			// Logged even on error: the counters describe what the run
			// did manage to delete before it stopped.
			deps.Logger.InfoContext(ctx, "traffic:purge",
				"uniques_deleted", res.UniquesDeleted,
				"paths_deleted", res.PathsDeleted,
				"referrers_deleted", res.ReferrersDeleted,
				"daily_deleted", res.DailyDeleted,
				"remaining", res.Remaining)
		}
		if err != nil {
			return err
		}
		// A run that stopped on the batch cap has rows left over. Kick
		// another pass now rather than waiting for tomorrow's cron beat,
		// so the first run after deploy drains the backlog in one night.
		if res.Remaining {
			if _, err := worker.Enqueue(ctx, deps.Pool, repotraffic.KindTrafficPurge,
				p, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "traffic:purge self-enqueue failed", "error", err)
			}
		}
		return nil
	}
}
