-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertWorkflowStep :one
INSERT INTO workflow_steps (
    job_id, step_index, step_id, step_name, if_expr,
    run_command, uses_alias, working_directory, step_env, continue_on_error,
    step_with
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
RETURNING id, job_id, step_index, step_id, step_name, if_expr,
          run_command, uses_alias, working_directory, step_env,
          continue_on_error, status, conclusion, log_object_key,
          log_byte_count, started_at, completed_at, version,
          created_at, updated_at, step_with;

-- name: GetWorkflowStepByID :one
SELECT id, job_id, step_index, step_id, step_name, if_expr,
       run_command, uses_alias, working_directory, step_env,
       continue_on_error, status, conclusion, log_object_key,
       log_byte_count, started_at, completed_at, version,
       created_at, updated_at, step_with
FROM workflow_steps
WHERE id = $1;

-- name: UpdateWorkflowStepStatus :one
UPDATE workflow_steps
SET status = $2,
    conclusion = sqlc.narg(conclusion)::check_conclusion,
    started_at = sqlc.narg(started_at)::timestamptz,
    completed_at = sqlc.narg(completed_at)::timestamptz,
    version = version + 1,
    updated_at = now()
WHERE id = $1
RETURNING id, job_id, step_index, step_id, step_name, if_expr,
          run_command, uses_alias, working_directory, step_env,
          continue_on_error, status, conclusion, log_object_key,
          log_byte_count, started_at, completed_at, version,
          created_at, updated_at, step_with;

-- name: CancelOpenWorkflowStepsForJob :many
UPDATE workflow_steps
SET status = 'cancelled',
    conclusion = 'cancelled',
    started_at = COALESCE(started_at, now()),
    completed_at = COALESCE(completed_at, now()),
    version = version + 1,
    updated_at = now()
WHERE job_id = $1
  AND status IN ('queued', 'running')
RETURNING id, job_id, step_index, step_id, step_name, if_expr,
          run_command, uses_alias, working_directory, step_env,
          continue_on_error, status, conclusion, log_object_key,
          log_byte_count, started_at, completed_at, version,
          created_at, updated_at, step_with;

-- name: UpdateWorkflowStepLogObject :one
UPDATE workflow_steps
SET log_object_key = sqlc.narg(log_object_key)::text,
    log_byte_count = sqlc.arg(log_byte_count)::bigint,
    version = version + 1,
    updated_at = now()
WHERE id = sqlc.arg(id)::bigint
RETURNING id, job_id, step_index, step_id, step_name, if_expr,
          run_command, uses_alias, working_directory, step_env,
          continue_on_error, status, conclusion, log_object_key,
          log_byte_count, started_at, completed_at, version,
          created_at, updated_at, step_with;

-- name: ListRunnerStepsForJob :many
SELECT id, job_id, step_index, step_id, step_name, if_expr,
       run_command, uses_alias, working_directory, step_env,
       continue_on_error, status, conclusion, log_byte_count,
       started_at, completed_at, created_at, step_with
FROM workflow_steps
WHERE job_id = $1
ORDER BY step_index ASC;

-- name: GetFirstStepForJob :one
SELECT id, job_id, step_index, step_id, step_name, if_expr,
       run_command, uses_alias, working_directory, step_env,
       continue_on_error, status, conclusion, log_object_key,
       log_byte_count, started_at, completed_at, version,
       created_at, updated_at, step_with
FROM workflow_steps
WHERE job_id = $1
ORDER BY step_index ASC
LIMIT 1;

-- name: ListStepsForJob :many
SELECT id, job_id, step_index, step_id, step_name, run_command,
       uses_alias, status, conclusion, log_object_key, log_byte_count,
       started_at, completed_at, created_at, updated_at
FROM workflow_steps
WHERE job_id = $1
ORDER BY step_index ASC;
