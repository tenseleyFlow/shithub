// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ObserveActions starts a goroutine that periodically refreshes DB-backed
// Actions gauges. The goroutine exits when ctx is canceled.
func ObserveActions(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	if pool == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	go func() {
		refreshActions(ctx, pool)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refreshActions(ctx, pool)
			}
		}
	}()
}

func refreshActions(ctx context.Context, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	refreshActionQueueGauges(ctx, pool)
	refreshActionRunnerGauges(ctx, pool)
	refreshActionStorageGauges(ctx, pool)
}

func refreshActionQueueGauges(ctx context.Context, pool *pgxpool.Pool) {
	ActionsQueueDepth.WithLabelValues("runs").Set(0)
	ActionsQueueDepth.WithLabelValues("jobs").Set(0)
	ActionsActive.WithLabelValues("runs").Set(0)
	ActionsActive.WithLabelValues("jobs").Set(0)

	rows, err := pool.Query(ctx, `
SELECT 'runs'::text AS resource, status::text, count(*)::double precision
FROM workflow_runs
WHERE status IN ('queued', 'running')
GROUP BY status
UNION ALL
SELECT 'jobs'::text AS resource, status::text, count(*)::double precision
FROM workflow_jobs
WHERE status IN ('queued', 'running')
GROUP BY status`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var resource, status string
		var count float64
		if err := rows.Scan(&resource, &status, &count); err != nil {
			return
		}
		switch status {
		case "queued":
			ActionsQueueDepth.WithLabelValues(resource).Set(count)
		case "running":
			ActionsActive.WithLabelValues(resource).Set(count)
		}
	}
}

func refreshActionRunnerGauges(ctx context.Context, pool *pgxpool.Pool) {
	ActionsRunnerHeartbeatAgeSeconds.Reset()
	ActionsRunnerCapacity.Reset()
	rows, err := pool.Query(ctx, `
SELECT name::text,
       status::text,
       capacity::double precision,
       EXTRACT(EPOCH FROM (now() - last_heartbeat_at))::double precision AS heartbeat_age_seconds
FROM workflow_runners
WHERE last_heartbeat_at IS NOT NULL`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name, status string
		var capacity, age float64
		if err := rows.Scan(&name, &status, &capacity, &age); err != nil {
			return
		}
		ActionsRunnerCapacity.WithLabelValues(name, status).Set(capacity)
		ActionsRunnerHeartbeatAgeSeconds.WithLabelValues(name, status).Set(age)
	}
}

func refreshActionStorageGauges(ctx context.Context, pool *pgxpool.Pool) {
	ActionsStorageObjects.WithLabelValues("artifacts").Set(0)
	ActionsStorageObjects.WithLabelValues("step_logs").Set(0)
	ActionsStorageObjects.WithLabelValues("hot_log_chunks").Set(0)
	ActionsStorageBytes.WithLabelValues("artifacts").Set(0)
	ActionsStorageBytes.WithLabelValues("step_logs").Set(0)
	ActionsStorageBytes.WithLabelValues("hot_log_chunks").Set(0)

	rows, err := pool.Query(ctx, `
SELECT 'artifacts'::text AS kind, count(*)::double precision, COALESCE(sum(byte_count), 0)::double precision
FROM workflow_artifacts
UNION ALL
SELECT 'step_logs'::text AS kind, count(*)::double precision, COALESCE(sum(log_byte_count), 0)::double precision
FROM workflow_steps
WHERE log_object_key IS NOT NULL
UNION ALL
SELECT 'hot_log_chunks'::text AS kind, count(*)::double precision, COALESCE(sum(octet_length(chunk)), 0)::double precision
FROM workflow_step_log_chunks`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var objects, bytes float64
		if err := rows.Scan(&kind, &objects, &bytes); err != nil {
			return
		}
		ActionsStorageObjects.WithLabelValues(kind).Set(objects)
		ActionsStorageBytes.WithLabelValues(kind).Set(bytes)
	}
}
