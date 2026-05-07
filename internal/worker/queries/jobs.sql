-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Jobs queue. The dispatch query uses FOR UPDATE SKIP LOCKED — the
-- canonical Postgres pattern for safe concurrent dequeue. Multiple
-- workers can run ClaimJob concurrently; each gets a different row.

-- name: EnqueueJob :one
-- Insert a new job. run_at defaults to now() so the job is immediately
-- runnable; pass a future timestamp for delayed work. The returned row
-- carries the assigned ID, which callers persist when the enqueue is
-- done inside a wider transaction.
INSERT INTO jobs (kind, payload, run_at, max_attempts)
VALUES ($1, $2, COALESCE(sqlc.narg(run_at)::timestamptz, now()), COALESCE(sqlc.narg(max_attempts)::int, 5))
RETURNING *;

-- name: ClaimJob :one
-- Atomically claim the oldest runnable job of a given kind. Workers
-- supply their own instance ID as locked_by so admins can see who's
-- holding what. Returns pgx.ErrNoRows when the queue is empty for
-- that kind.
UPDATE jobs
SET locked_by = $2,
    locked_at = now(),
    attempts  = jobs.attempts + 1
WHERE id = (
    SELECT j.id FROM jobs j
    WHERE j.kind         = $1
      AND j.completed_at IS NULL
      AND j.failed_at    IS NULL
      AND j.run_at       <= now()
      AND (j.locked_by IS NULL OR j.locked_at < now() - interval '5 minutes')
    ORDER BY j.run_at ASC, j.id ASC
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
RETURNING *;

-- name: MarkJobCompleted :exec
-- On success: clear lock + set completed_at.
UPDATE jobs
SET completed_at = now(),
    locked_by    = NULL,
    locked_at    = NULL
WHERE id = $1;

-- name: RescheduleJob :exec
-- Transient failure: clear the lock, record the error, push run_at
-- forward by the caller-computed backoff. attempts was incremented
-- by ClaimJob, so checking attempts >= max_attempts elsewhere decides
-- between Reschedule and MarkJobFailed.
UPDATE jobs
SET locked_by  = NULL,
    locked_at  = NULL,
    last_error = $2,
    run_at     = $3
WHERE id = $1;

-- name: MarkJobFailed :exec
-- Terminal failure (attempts hit max). Holds locked_by NULL so the
-- row is no longer "in flight" to dashboards; failed_at marks it
-- visible to a future poison-job inspector (S34 admin panel).
UPDATE jobs
SET failed_at  = now(),
    locked_by  = NULL,
    locked_at  = NULL,
    last_error = $2
WHERE id = $1;

-- name: PurgeCompletedJobs :execrows
-- Delete completed jobs older than the supplied cutoff. Used by the
-- jobs:purge_completed maintenance job. Returns the number of rows
-- deleted so the cron handler can log progress.
DELETE FROM jobs
WHERE completed_at IS NOT NULL
  AND completed_at < $1;

-- name: PurgeFailedJobs :execrows
-- Same as PurgeCompletedJobs but for terminally failed rows. Kept
-- separate because operators usually want a longer retention on
-- failures to inspect what blew up.
DELETE FROM jobs
WHERE failed_at IS NOT NULL
  AND failed_at < $1;

-- name: GetJob :one
-- Single-row lookup by id. Used in tests and the admin panel.
SELECT * FROM jobs WHERE id = $1;
