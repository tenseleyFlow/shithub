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
          started_at, completed_at, version, created_at, updated_at;

-- name: GetWorkflowRunByID :one
SELECT id, repo_id, run_index, workflow_file, workflow_name,
       head_sha, head_ref, event, event_payload,
       actor_user_id, parent_run_id, concurrency_group,
       status, conclusion, pinned, need_approval, approved_by_user_id,
       started_at, completed_at, version, created_at, updated_at
FROM workflow_runs
WHERE id = $1;

-- name: NextRunIndexForRepo :one
-- Atomic next-index emitter: take the max + 1 for this repo. Pairs
-- with the (repo_id, run_index) UNIQUE so concurrent inserts that
-- race here will catch a unique-violation and the caller retries.
SELECT COALESCE(MAX(run_index), 0) + 1 AS next_index
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
