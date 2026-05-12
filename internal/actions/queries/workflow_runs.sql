-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertWorkflowRun :one
INSERT INTO workflow_runs (
    repo_id, run_index, workflow_file, workflow_name,
    head_sha, head_ref, event, event_payload,
    actor_user_id, parent_run_id, concurrency_group, need_approval
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING id, repo_id, run_index, workflow_file, workflow_name,
          head_sha, head_ref, event, event_payload,
          actor_user_id, parent_run_id, concurrency_group,
          status, conclusion, pinned, need_approval, approved_by_user_id,
          started_at, completed_at, version, created_at, updated_at, trigger_event_id;

-- name: EnqueueWorkflowRun :one
-- Idempotent insert: if a row with the same (repo_id, workflow_file,
-- trigger_event_id) already exists, returns no rows (pgx.ErrNoRows in
-- Go). The handler treats that as a successful no-op so worker
-- retries and admin replays of the same triggering event don't
-- duplicate runs.
--
-- The ON CONFLICT predicate matches the partial unique index defined
-- in migration 0051; both must agree for postgres to infer the
-- target.
INSERT INTO workflow_runs (
    repo_id, run_index, workflow_file, workflow_name,
    head_sha, head_ref, event, event_payload,
    actor_user_id, parent_run_id, concurrency_group, need_approval,
    trigger_event_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (repo_id, workflow_file, trigger_event_id) WHERE trigger_event_id <> ''
DO NOTHING
RETURNING id, repo_id, run_index, workflow_file, workflow_name,
          head_sha, head_ref, event, event_payload,
          actor_user_id, parent_run_id, concurrency_group,
          status, conclusion, pinned, need_approval, approved_by_user_id,
          started_at, completed_at, version, created_at, updated_at, trigger_event_id;

-- name: LookupWorkflowRunByTriggerEvent :one
-- Companion to EnqueueWorkflowRun for the conflict path: when an
-- INSERT ... ON CONFLICT DO NOTHING returns no rows, the trigger
-- handler uses this to find the existing row so it can surface a
-- stable RunID. Matches the partial-unique index from migration 0051.
SELECT id, repo_id, run_index, workflow_file, workflow_name,
       head_sha, head_ref, event, event_payload,
       actor_user_id, parent_run_id, concurrency_group,
       status, conclusion, pinned, need_approval, approved_by_user_id,
       started_at, completed_at, version, created_at, updated_at, trigger_event_id
FROM workflow_runs
WHERE repo_id = $1 AND workflow_file = $2 AND trigger_event_id = $3;

-- name: GetWorkflowRunByID :one
SELECT id, repo_id, run_index, workflow_file, workflow_name,
       head_sha, head_ref, event, event_payload,
       actor_user_id, parent_run_id, concurrency_group,
       status, conclusion, pinned, need_approval, approved_by_user_id,
       started_at, completed_at, version, created_at, updated_at, trigger_event_id
FROM workflow_runs
WHERE id = $1;

-- name: ListBlockingConcurrencyRunsForUpdate :many
-- Older queued/running runs with the same group block the new run while they
-- still have at least one queued/running job that has not already received a
-- cancel request. cancel-in-progress releases the slot by flipping that job
-- flag even if the runner is still draining the old container.
SELECT r.id, r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
       r.head_sha, r.head_ref, r.event, r.event_payload,
       r.actor_user_id, r.parent_run_id, r.concurrency_group,
       r.status, r.conclusion, r.pinned, r.need_approval, r.approved_by_user_id,
       r.started_at, r.completed_at, r.version, r.created_at, r.updated_at, r.trigger_event_id
FROM workflow_runs r
JOIN workflow_runs current_run ON current_run.id = sqlc.arg(run_id)::bigint
WHERE r.repo_id = sqlc.arg(repo_id)::bigint
  AND r.concurrency_group = sqlc.arg(concurrency_group)::text
  AND r.concurrency_group <> ''
  AND r.id <> current_run.id
  AND r.status IN ('queued', 'running')
  AND (r.created_at, r.id) < (current_run.created_at, current_run.id)
  AND EXISTS (
      SELECT 1
      FROM workflow_jobs j
      WHERE j.run_id = r.id
        AND j.status IN ('queued', 'running')
        AND j.cancel_requested = false
  )
ORDER BY r.created_at ASC, r.id ASC
FOR UPDATE OF r;

-- name: GetWorkflowRunForRepoByIndex :one
SELECT r.id, r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
       r.head_sha, r.head_ref, r.event, r.event_payload,
       r.actor_user_id, COALESCE(u.username::text, '')::text AS actor_username,
       r.parent_run_id, r.concurrency_group,
       r.status, r.conclusion, r.pinned, r.need_approval, r.approved_by_user_id,
       r.started_at, r.completed_at, r.version, r.created_at, r.updated_at, r.trigger_event_id
FROM workflow_runs r
LEFT JOIN users u ON u.id = r.actor_user_id
WHERE r.repo_id = $1 AND r.run_index = $2;

-- name: MarkWorkflowRunRunning :exec
UPDATE workflow_runs
SET status = 'running',
    started_at = COALESCE(started_at, now()),
    version = version + 1,
    updated_at = now()
WHERE id = $1 AND status = 'queued';

-- name: StartWorkflowRun :one
UPDATE workflow_runs
SET status = 'running',
    started_at = COALESCE(started_at, now()),
    version = version + 1,
    updated_at = now()
WHERE id = $1 AND status = 'queued'
RETURNING id, repo_id, run_index, workflow_file, workflow_name,
          head_sha, head_ref, event, event_payload,
          actor_user_id, parent_run_id, concurrency_group,
          status, conclusion, pinned, need_approval, approved_by_user_id,
          started_at, completed_at, version, created_at, updated_at, trigger_event_id;

-- name: CompleteWorkflowRun :one
UPDATE workflow_runs
SET status = 'completed',
    conclusion = sqlc.arg(conclusion)::check_conclusion,
    started_at = COALESCE(started_at, now()),
    completed_at = COALESCE(completed_at, now()),
    version = version + 1,
    updated_at = now()
WHERE id = $1
RETURNING id, repo_id, run_index, workflow_file, workflow_name,
          head_sha, head_ref, event, event_payload,
          actor_user_id, parent_run_id, concurrency_group,
          status, conclusion, pinned, need_approval, approved_by_user_id,
          started_at, completed_at, version, created_at, updated_at, trigger_event_id;

-- name: NextRunIndexForRepo :one
-- Atomic next-index emitter: take the max + 1 for this repo. Pairs
-- with the (repo_id, run_index) UNIQUE so concurrent inserts that
-- race here will catch a unique-violation and the caller retries.
-- Cast to bigint so sqlc generates int64 (the column type) rather
-- than int32 (the type the +1 literal would default to).
SELECT (COALESCE(MAX(run_index), 0) + 1)::bigint AS next_index
FROM workflow_runs
WHERE repo_id = $1;

-- name: ListWorkflowRunsForRepo :many
SELECT r.id, r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
       r.head_sha, r.head_ref, r.event, r.status, r.conclusion,
       r.actor_user_id, COALESCE(u.username::text, '')::text AS actor_username,
       r.started_at, r.completed_at, r.created_at, r.updated_at
FROM workflow_runs r
LEFT JOIN users u ON u.id = r.actor_user_id
WHERE r.repo_id = sqlc.arg(repo_id)::bigint
  AND (sqlc.narg(workflow_file)::text IS NULL OR r.workflow_file = sqlc.narg(workflow_file)::text)
  AND (sqlc.narg(head_ref)::text IS NULL OR r.head_ref = sqlc.narg(head_ref)::text)
  AND (sqlc.narg(event)::workflow_run_event IS NULL OR r.event = sqlc.narg(event)::workflow_run_event)
  AND (sqlc.narg(status)::workflow_run_status IS NULL OR r.status = sqlc.narg(status)::workflow_run_status)
  AND (sqlc.narg(conclusion)::check_conclusion IS NULL OR r.conclusion = sqlc.narg(conclusion)::check_conclusion)
  AND (sqlc.narg(actor_username)::text IS NULL OR u.username = sqlc.narg(actor_username)::citext)
ORDER BY r.created_at DESC, r.id DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountWorkflowRunsForRepo :one
SELECT COUNT(*)::bigint
FROM workflow_runs r
LEFT JOIN users u ON u.id = r.actor_user_id
WHERE r.repo_id = sqlc.arg(repo_id)::bigint
  AND (sqlc.narg(workflow_file)::text IS NULL OR r.workflow_file = sqlc.narg(workflow_file)::text)
  AND (sqlc.narg(head_ref)::text IS NULL OR r.head_ref = sqlc.narg(head_ref)::text)
  AND (sqlc.narg(event)::workflow_run_event IS NULL OR r.event = sqlc.narg(event)::workflow_run_event)
  AND (sqlc.narg(status)::workflow_run_status IS NULL OR r.status = sqlc.narg(status)::workflow_run_status)
  AND (sqlc.narg(conclusion)::check_conclusion IS NULL OR r.conclusion = sqlc.narg(conclusion)::check_conclusion)
  AND (sqlc.narg(actor_username)::text IS NULL OR u.username = sqlc.narg(actor_username)::citext);

-- name: ListActiveWorkflowRunsForAdmin :many
SELECT id, repo_id, run_index, workflow_file, workflow_name,
       head_sha, head_ref, event, event_payload,
       actor_user_id, parent_run_id, concurrency_group,
       status, conclusion, pinned, need_approval, approved_by_user_id,
       started_at, completed_at, version, created_at, updated_at, trigger_event_id
FROM workflow_runs
WHERE status IN ('queued', 'running')
  AND (sqlc.arg(repo_id)::bigint = 0 OR repo_id = sqlc.arg(repo_id)::bigint)
ORDER BY created_at ASC, id ASC
LIMIT sqlc.arg(limit_count)::int;

-- name: ListWorkflowRunWorkflowsForRepo :many
WITH ranked AS (
    SELECT workflow_file,
           workflow_name,
           (COUNT(*) OVER (PARTITION BY workflow_file))::bigint AS run_count,
           ROW_NUMBER() OVER (PARTITION BY workflow_file ORDER BY created_at DESC, id DESC) AS rn
    FROM workflow_runs
    WHERE repo_id = $1
)
SELECT workflow_file,
       COALESCE(NULLIF(workflow_name, ''), workflow_file) AS workflow_name,
       run_count
FROM ranked
WHERE rn = 1
ORDER BY lower(COALESCE(NULLIF(workflow_name, ''), workflow_file)), workflow_file;

-- name: DeleteOldWorkflowRunsForCleanup :execrows
DELETE FROM workflow_runs
WHERE pinned = false
  AND status IN ('completed', 'cancelled')
  AND completed_at IS NOT NULL
  AND completed_at < $1;
