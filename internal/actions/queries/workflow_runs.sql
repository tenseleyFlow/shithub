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
SELECT id, repo_id, run_index, workflow_file, workflow_name,
       head_sha, head_ref, event, status, conclusion,
       actor_user_id, started_at, completed_at, created_at
FROM workflow_runs
WHERE repo_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
