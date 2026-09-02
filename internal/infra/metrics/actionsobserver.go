// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const actionsRunnerStaleAfter = 60 * time.Second

// defaultActionsInterval is the cadence for the cheap gauges when the caller
// does not pick one.
const defaultActionsInterval = 15 * time.Second

// actionsStorageBytesInterval bounds how often the hot log-chunk byte sum
// runs. `sum(octet_length(chunk))` has to scan and detoast every row of
// workflow_step_log_chunks, which costs the same whether or not anything is
// running; at the 15s cadence of the other gauges it was a standing load on
// the database. Chunk volume moves slowly enough that a 5 minute gauge is
// still useful.
const actionsStorageBytesInterval = 5 * time.Minute

// ObserveActions starts a goroutine that periodically refreshes DB-backed
// Actions gauges. The goroutine exits when ctx is canceled.
//
// interval drives the queue, runner and object-count gauges. The hot
// log-chunk byte sum is refreshed on the slower actionsStorageBytesInterval
// cadence; see refreshActionLogChunkBytes.
func ObserveActions(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	if pool == nil {
		return
	}
	if interval <= 0 {
		interval = defaultActionsInterval
	}
	slowEvery := ticksBetween(interval, actionsStorageBytesInterval)
	t := time.NewTicker(interval)
	go func() {
		defer t.Stop()
		observeActionsLoop(ctx, t.C, slowEvery,
			func(ctx context.Context) { refreshActionsFast(ctx, pool) },
			func(ctx context.Context) { refreshActionLogChunkBytes(ctx, pool) },
		)
	}()
}

// ticksBetween returns how many ticks of length tick must elapse between two
// runs of a task that should run at most once per every. It rounds up, so the
// task never runs more often than requested, and never returns less than 1.
func ticksBetween(tick, every time.Duration) int {
	if tick <= 0 || every <= tick {
		return 1
	}
	n := int((every + tick - 1) / tick)
	if n < 1 {
		return 1
	}
	return n
}

// observeActionsLoop runs fast on every tick and slow once every slowEvery
// ticks. Both run once up front so the gauges are populated before the first
// tick. It returns when ctx is canceled or ticks is closed.
func observeActionsLoop(ctx context.Context, ticks <-chan time.Time, slowEvery int, fast, slow func(context.Context)) {
	if slowEvery < 1 {
		slowEvery = 1
	}
	fast(ctx)
	slow(ctx)
	sinceSlow := 0
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			fast(ctx)
			sinceSlow++
			if sinceSlow >= slowEvery {
				sinceSlow = 0
				slow(ctx)
			}
		}
	}
}

// refreshActions refreshes every Actions gauge, cheap and expensive alike.
// The observer loop splits the two cadences apart; this is the one-shot form.
func refreshActions(ctx context.Context, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	refreshActionsFast(ctx, pool)
	refreshActionLogChunkBytes(ctx, pool)
}

func refreshActionsFast(ctx context.Context, pool *pgxpool.Pool) {
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
	ActionsQueueDepthByLabels.Reset()
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
	rows.Close()

	labelRows, err := pool.Query(ctx, `
SELECT COALESCE(NULLIF(runs_on, ''), '(none)')::text AS labels,
       count(*)::double precision
FROM workflow_jobs
WHERE status = 'queued'
  AND cancel_requested = false
  AND runner_id IS NULL
GROUP BY COALESCE(NULLIF(runs_on, ''), '(none)')`)
	if err != nil {
		return
	}
	defer labelRows.Close()
	for labelRows.Next() {
		var labels string
		var count float64
		if err := labelRows.Scan(&labels, &count); err != nil {
			return
		}
		ActionsQueueDepthByLabels.WithLabelValues(labels).Set(count)
	}
}

func refreshActionRunnerGauges(ctx context.Context, pool *pgxpool.Pool) {
	ActionsRunnerHeartbeatAgeSeconds.Reset()
	ActionsRunnerOnline.Reset()
	ActionsRunnerDraining.Reset()
	ActionsRunnerCapacity.Reset()
	ActionsRunnerActiveJobs.Reset()
	ActionsRunnerStaleTotal.Set(0)
	rows, err := pool.Query(ctx, `
SELECT r.name::text,
       r.status::text,
       r.capacity::double precision,
       COALESCE(EXTRACT(EPOCH FROM (now() - r.last_heartbeat_at))::double precision, -1) AS heartbeat_age_seconds,
       (r.draining_at IS NOT NULL)::boolean AS draining,
       (r.revoked_at IS NOT NULL)::boolean AS revoked,
       COUNT(j.id)::double precision AS active_jobs
FROM workflow_runners r
LEFT JOIN workflow_jobs j ON j.runner_id = r.id AND j.status = 'running'
GROUP BY r.id, r.name, r.status, r.capacity, r.last_heartbeat_at, r.draining_at, r.revoked_at`)
	if err != nil {
		return
	}
	defer rows.Close()
	var stale float64
	for rows.Next() {
		var name, status string
		var capacity, age, activeJobs float64
		var draining, revoked bool
		if err := rows.Scan(&name, &status, &capacity, &age, &draining, &revoked, &activeJobs); err != nil {
			return
		}
		ActionsRunnerCapacity.WithLabelValues(name, status).Set(capacity)
		ActionsRunnerActiveJobs.WithLabelValues(name, status).Set(activeJobs)
		if age >= 0 {
			ActionsRunnerHeartbeatAgeSeconds.WithLabelValues(name, status).Set(age)
		}
		online := !revoked && status != "offline" && age >= 0 && age <= actionsRunnerStaleAfter.Seconds()
		if online {
			ActionsRunnerOnline.WithLabelValues(name).Set(1)
		} else {
			ActionsRunnerOnline.WithLabelValues(name).Set(0)
		}
		if draining {
			ActionsRunnerDraining.WithLabelValues(name).Set(1)
		} else {
			ActionsRunnerDraining.WithLabelValues(name).Set(0)
		}
		if !revoked && status != "offline" && age > actionsRunnerStaleAfter.Seconds() {
			stale++
		}
	}
	ActionsRunnerStaleTotal.Set(stale)
}

// refreshActionStorageGauges publishes the object counts for all three storage
// kinds plus the two byte sums that read a plain integer column. The
// hot_log_chunks byte sum is deliberately absent: it is the only one that has
// to detoast, so refreshActionLogChunkBytes owns that gauge.
func refreshActionStorageGauges(ctx context.Context, pool *pgxpool.Pool) {
	ActionsStorageObjects.WithLabelValues("artifacts").Set(0)
	ActionsStorageObjects.WithLabelValues("step_logs").Set(0)
	ActionsStorageObjects.WithLabelValues("hot_log_chunks").Set(0)
	ActionsStorageBytes.WithLabelValues("artifacts").Set(0)
	ActionsStorageBytes.WithLabelValues("step_logs").Set(0)

	rows, err := pool.Query(ctx, `
SELECT 'artifacts'::text AS kind, count(*)::double precision, COALESCE(sum(byte_count), 0)::double precision
FROM workflow_artifacts
UNION ALL
SELECT 'step_logs'::text AS kind, count(*)::double precision, COALESCE(sum(log_byte_count), 0)::double precision
FROM workflow_steps
WHERE log_object_key IS NOT NULL
UNION ALL
SELECT 'hot_log_chunks'::text AS kind, count(*)::double precision, 0::double precision
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
		if kind == "hot_log_chunks" {
			continue
		}
		ActionsStorageBytes.WithLabelValues(kind).Set(bytes)
	}
}

// refreshActionLogChunkBytes publishes shithub_actions_storage_bytes for the
// hot chunk table. Every row is a bytea that Postgres has to fetch out of the
// TOAST heap to measure, so this runs on actionsStorageBytesInterval rather
// than with the cheap gauges.
func refreshActionLogChunkBytes(ctx context.Context, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	var bytes float64
	err := pool.QueryRow(ctx, `
SELECT COALESCE(sum(octet_length(chunk)), 0)::double precision
FROM workflow_step_log_chunks`).Scan(&bytes)
	if err != nil {
		return
	}
	ActionsStorageBytes.WithLabelValues("hot_log_chunks").Set(bytes)
}
