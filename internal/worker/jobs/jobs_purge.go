// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/worker"
	workerdb "github.com/tenseleyFlow/shithub/internal/worker/sqlc"
)

// JobsPurgeDeps wires the purge handler.
type JobsPurgeDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// JobsPurgePayload — empty object is fine; defaults below.
type JobsPurgePayload struct {
	CompletedOlderThanDays int `json:"completed_older_than_days,omitempty"`
	FailedOlderThanDays    int `json:"failed_older_than_days,omitempty"`
}

// JobsPurge deletes completed/failed jobs older than the configured
// retention. Retention defaults: 14 days completed, 30 days failed.
// Designed to run as a cron job (S26 ships scheduling); for now it can
// be enqueued ad-hoc by the operator.
func JobsPurge(deps JobsPurgeDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		p := JobsPurgePayload{}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &p) // tolerant of empty/malformed; we have defaults
		}
		if p.CompletedOlderThanDays <= 0 {
			p.CompletedOlderThanDays = 14
		}
		if p.FailedOlderThanDays <= 0 {
			p.FailedOlderThanDays = 30
		}
		now := time.Now()
		q := workerdb.New()
		completedCutoff := pgtype.Timestamptz{Time: now.Add(-time.Duration(p.CompletedOlderThanDays) * 24 * time.Hour), Valid: true}
		failedCutoff := pgtype.Timestamptz{Time: now.Add(-time.Duration(p.FailedOlderThanDays) * 24 * time.Hour), Valid: true}

		nC, err := q.PurgeCompletedJobs(ctx, deps.Pool, completedCutoff)
		if err != nil {
			return fmt.Errorf("purge completed: %w", err)
		}
		nF, err := q.PurgeFailedJobs(ctx, deps.Pool, failedCutoff)
		if err != nil {
			return fmt.Errorf("purge failed: %w", err)
		}
		deps.Logger.InfoContext(ctx, "jobs:purge",
			"completed_deleted", nC, "failed_deleted", nF)
		return nil
	}
}
