// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type TrendingComputeDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

type TrendingComputePayload struct {
	ScheduleNext    *bool `json:"schedule_next,omitempty"`
	IntervalMinutes int32 `json:"interval_minutes,omitempty"`
}

func TrendingCompute(deps TrendingComputeDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		if deps.Pool == nil {
			return errors.New("trending:compute: missing pool")
		}
		payload := TrendingComputePayload{}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &payload); err != nil {
				return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
			}
		}

		if err := social.CaptureTrendingSnapshots(ctx, social.Deps{Pool: deps.Pool, Logger: deps.Logger}); err != nil {
			return err
		}
		if deps.Logger != nil {
			deps.Logger.InfoContext(ctx, "trending:compute captured snapshots")
		}

		scheduleNext := true
		if payload.ScheduleNext != nil {
			scheduleNext = *payload.ScheduleNext
		}
		if !scheduleNext {
			return nil
		}
		interval := time.Duration(payload.IntervalMinutes) * time.Minute
		if interval <= 0 {
			interval = time.Hour
		}
		if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindTrendingCompute, map[string]any{
			"schedule_next":    true,
			"interval_minutes": int(interval / time.Minute),
		}, worker.EnqueueOptions{
			RunAt:       pgtype.Timestamptz{Time: time.Now().Add(interval), Valid: true},
			MaxAttempts: 3,
		}); err != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "trending:compute self-enqueue failed", "error", err)
			}
			return nil
		}
		_ = worker.Notify(ctx, deps.Pool)
		return nil
	}
}
