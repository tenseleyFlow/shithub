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

-- name: UpdateWorkflowJobStatus :one
UPDATE workflow_jobs
SET status = $2,
    conclusion = sqlc.narg(conclusion)::check_conclusion,
    started_at = sqlc.narg(started_at)::timestamptz,
    completed_at = sqlc.narg(completed_at)::timestamptz,
    version = version + 1,
    updated_at = now()
WHERE id = $1
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at;

-- name: CountRunningJobsForRunner :one
SELECT COUNT(*)::integer
FROM workflow_jobs
WHERE runner_id = sqlc.arg(runner_id)::bigint AND status = 'running';

-- name: ClaimQueuedWorkflowJob :one
WITH candidate AS (
    SELECT j.id
    FROM workflow_jobs j
    WHERE j.status = 'queued'
      AND j.runner_id IS NULL
      AND (j.runs_on = '' OR j.runs_on = ANY(sqlc.arg(labels)::text[]))
      AND NOT EXISTS (
          SELECT 1
          FROM workflow_jobs dep
          WHERE dep.run_id = j.run_id
            AND dep.job_key = ANY(j.needs_jobs)
            AND (dep.status <> 'completed' OR dep.conclusion <> 'success')
      )
    ORDER BY j.created_at ASC, j.id ASC
    FOR UPDATE OF j SKIP LOCKED
    LIMIT 1
),
claimed AS (
    UPDATE workflow_jobs j
    SET runner_id = sqlc.arg(runner_id)::bigint,
        status = 'running',
        started_at = COALESCE(j.started_at, now()),
        version = j.version + 1,
        updated_at = now()
    FROM candidate c
    WHERE j.id = c.id
    RETURNING j.id, j.run_id, j.job_index, j.job_key, j.job_name, j.runs_on,
              j.runner_id, j.needs_jobs, j.if_expr, j.timeout_minutes,
              j.permissions, j.job_env, j.status, j.conclusion,
              j.cancel_requested, j.started_at, j.completed_at, j.version,
              j.created_at, j.updated_at
)
SELECT c.id, c.run_id, c.job_index, c.job_key, c.job_name, c.runs_on,
       c.runner_id, c.needs_jobs, c.if_expr, c.timeout_minutes,
       c.permissions, c.job_env, c.status, c.conclusion,
       c.cancel_requested, c.started_at, c.completed_at, c.version,
       c.created_at, c.updated_at,
       r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
       r.head_sha, r.head_ref, r.event, r.event_payload
FROM claimed c
JOIN workflow_runs r ON r.id = c.run_id;

-- name: ListJobsForRun :many
SELECT id, run_id, job_index, job_key, job_name, runs_on, status,
       conclusion, started_at, completed_at, created_at
FROM workflow_jobs
WHERE run_id = $1
ORDER BY job_index ASC;
