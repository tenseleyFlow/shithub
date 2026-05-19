// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/repos/dependencyupdates"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

const DefaultDependencyUpdateSweepBatch = 50

type RepoDependencyUpdateSweepDeps struct {
	Pool      *pgxpool.Pool
	Logger    *slog.Logger
	Now       func() time.Time
	BatchSize int32
}

type dependencyUpdateSweepJobSummary struct {
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	NextRunAt string `json:"next_run_at,omitempty"`
}

type RepoDependencyUpdateRunPayload struct {
	JobID int64 `json:"job_id"`
}

// RepoDependencyUpdateSweep claims due dependency update configs and expands
// each one into a bounded domain job, then enqueues the update worker to
// consume that job.
func RepoDependencyUpdateSweep(deps RepoDependencyUpdateSweepDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("dependency update sweep: missing pool")
		}
		logger := deps.Logger
		if logger == nil {
			logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		now := deps.Now
		if now == nil {
			now = time.Now
		}
		batch := deps.BatchSize
		if batch <= 0 {
			batch = DefaultDependencyUpdateSweepBatch
		}

		processed, err := runDependencyUpdateSweep(ctx, deps.Pool, now().UTC(), batch)
		if err != nil {
			return err
		}
		if processed > 0 {
			if err := worker.Notify(ctx, deps.Pool); err != nil {
				logger.WarnContext(ctx, "dependency update sweep notify failed", "error", err)
			}
		}
		if processed > 0 {
			logger.InfoContext(ctx, "dependency update sweep drained configs", "count", processed)
		}
		if processed >= batch {
			if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindRepoDependencyUpdateSweep,
				map[string]any{}, worker.EnqueueOptions{}); err != nil {
				logger.WarnContext(ctx, "dependency update sweep self-enqueue failed", "error", err)
			}
		}
		return nil
	}
}

func runDependencyUpdateSweep(ctx context.Context, pool *pgxpool.Pool, now time.Time, batch int32) (int32, error) {
	q := reposdb.New()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	configs, err := q.ClaimDueDependencyUpdateConfigs(ctx, tx, reposdb.ClaimDueDependencyUpdateConfigsParams{
		NowAt:     pgtype.Timestamptz{Time: now.UTC(), Valid: true},
		LimitRows: batch,
	})
	if err != nil {
		return 0, err
	}
	for _, cfg := range configs {
		nextRunAt, status, summary, lastErr := nextDependencyUpdateSweepState(cfg, now)
		summaryJSON, err := json.Marshal(summary)
		if err != nil {
			summaryJSON = []byte(`{"status":"failed","message":"could not marshal summary"}`)
			status = "failed"
			lastErr = "could not marshal summary"
		}
		job, err := q.CreateDependencyUpdateJob(ctx, tx, reposdb.CreateDependencyUpdateJobParams{
			RepoID:        cfg.RepoID,
			ConfigID:      pgtype.Int8{Int64: cfg.ID, Valid: true},
			JobKind:       "version_update",
			Status:        status,
			TriggerSource: "schedule",
			ScheduledFor:  cfg.NextRunAt,
			ResultSummary: summaryJSON,
			LastError:     lastErr,
		})
		if err != nil {
			return 0, err
		}
		if status == "queued" {
			if _, err := worker.Enqueue(ctx, tx, worker.KindRepoDependencyUpdateRun,
				RepoDependencyUpdateRunPayload{JobID: job.ID}, worker.EnqueueOptions{}); err != nil {
				return 0, err
			}
		}
		if _, err := q.TouchDependencyUpdateConfigChecked(ctx, tx, reposdb.TouchDependencyUpdateConfigCheckedParams{
			ID:        cfg.ID,
			NextRunAt: nextRunAt,
		}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int32(len(configs)), nil
}

func nextDependencyUpdateSweepState(cfg reposdb.DependencyUpdateConfig, now time.Time) (pgtype.Timestamptz, string, dependencyUpdateSweepJobSummary, string) {
	next, err := dependencyupdates.NextRunAfter(dependencyUpdateScheduleFromRow(cfg), now, dependencyUpdateScheduleSeedFromRow(cfg))
	if err != nil {
		msg := err.Error()
		return pgtype.Timestamptz{}, "failed", dependencyUpdateSweepJobSummary{
			Status:  "failed",
			Message: msg,
		}, msg
	}
	return pgtype.Timestamptz{Time: next.UTC(), Valid: true}, "queued", dependencyUpdateSweepJobSummary{
		Status:    "queued",
		Message:   "waiting for dependency update worker",
		NextRunAt: next.UTC().Format(time.RFC3339),
	}, ""
}

func dependencyUpdateScheduleFromRow(cfg reposdb.DependencyUpdateConfig) dependencyupdates.Schedule {
	return dependencyupdates.Schedule{
		Interval: cfg.ScheduleInterval,
		Day:      cfg.ScheduleDay,
		Time:     cfg.ScheduleTime,
		Timezone: cfg.ScheduleTimezone,
		Cronjob:  cfg.ScheduleCron,
	}
}

func dependencyUpdateScheduleSeedFromRow(cfg reposdb.DependencyUpdateConfig) string {
	return cfg.RawConfigHash + "|" + cfg.Ecosystem + "|" + cfg.Directory
}
