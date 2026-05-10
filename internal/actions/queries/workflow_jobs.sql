-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertWorkflowJob :one
INSERT INTO workflow_jobs (
    run_id, job_index, job_key, job_name,
    runs_on, needs_jobs, if_expr, timeout_minutes,
    permissions, job_env
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at;

-- name: GetWorkflowJobByID :one
SELECT id, run_id, job_index, job_key, job_name, runs_on,
       runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
       job_env, status, conclusion, cancel_requested,
       started_at, completed_at, version, created_at, updated_at
FROM workflow_jobs
WHERE id = $1;

-- name: ListJobsForRun :many
SELECT id, run_id, job_index, job_key, job_name, runs_on, status,
       conclusion, started_at, completed_at, created_at
FROM workflow_jobs
WHERE run_id = $1
ORDER BY job_index ASC;
