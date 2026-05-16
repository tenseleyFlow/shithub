// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// CronWorkflowSweepDeps wires the sweep handler.
type CronWorkflowSweepDeps struct {
	Pool           *pgxpool.Pool
	Logger         *slog.Logger
	RepoFS         *storage.RepoFS
	BillingEnforce config.EnforceConfig
}

// CronWorkflowSweep claims a batch of due cron dispatches and fires
// each one. Self-throttles: if a batch fills the cap, re-enqueues
// itself so the next tick keeps draining without waiting for the
// systemd timer.
func CronWorkflowSweep(deps CronWorkflowSweepDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		n, err := cronworkflow.SweepOnce(ctx, cronworkflow.FireDeps{
			Pool:           deps.Pool,
			Logger:         deps.Logger,
			RepoFS:         deps.RepoFS,
			BillingEnforce: deps.BillingEnforce,
		})
		if err != nil {
			return err
		}
		if deps.Logger != nil && n > 0 {
			deps.Logger.InfoContext(ctx, "cronworkflow: sweep drained dispatches",
				"count", n)
		}
		if n >= cronworkflow.SweepBatch {
			if _, err := worker.Enqueue(ctx, deps.Pool, cronworkflow.KindCronWorkflowSweep,
				map[string]any{}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "cronworkflow: sweep self-enqueue failed",
					"error", err)
			}
		}
		return nil
	}
}
