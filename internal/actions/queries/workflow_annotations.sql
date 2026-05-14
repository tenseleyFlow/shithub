-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertWorkflowAnnotation :one
INSERT INTO workflow_annotations (
    run_id, job_id, step_id, level, title, message, path,
    start_line, end_line, start_column, end_column,
    log_line, log_chunk_seq, fingerprint
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11,
    $12, $13, $14
)
ON CONFLICT (step_id, fingerprint) DO UPDATE SET
    run_id = EXCLUDED.run_id,
    job_id = EXCLUDED.job_id,
    level = EXCLUDED.level,
    title = EXCLUDED.title,
    message = EXCLUDED.message,
    path = EXCLUDED.path,
    start_line = EXCLUDED.start_line,
    end_line = EXCLUDED.end_line,
    start_column = EXCLUDED.start_column,
    end_column = EXCLUDED.end_column,
    log_line = EXCLUDED.log_line,
    log_chunk_seq = EXCLUDED.log_chunk_seq
RETURNING id, run_id, job_id, step_id, level, title, message, path,
          start_line, end_line, start_column, end_column,
          log_line, log_chunk_seq, fingerprint, created_at;

-- name: ListWorkflowAnnotationsForRun :many
SELECT
    a.id, a.run_id, a.job_id, a.step_id, a.level, a.title, a.message, a.path,
    a.start_line, a.end_line, a.start_column, a.end_column,
    a.log_line, a.log_chunk_seq, a.fingerprint, a.created_at,
    j.job_index, j.job_key, j.job_name,
    s.step_index, s.step_name, s.run_command, s.uses_alias
FROM workflow_annotations a
JOIN workflow_jobs j ON j.id = a.job_id
JOIN workflow_steps s ON s.id = a.step_id
WHERE a.run_id = $1
ORDER BY j.job_index ASC, s.step_index ASC, a.created_at ASC, a.id ASC;

-- name: ListWorkflowAnnotationsForStep :many
SELECT
    a.id, a.run_id, a.job_id, a.step_id, a.level, a.title, a.message, a.path,
    a.start_line, a.end_line, a.start_column, a.end_column,
    a.log_line, a.log_chunk_seq, a.fingerprint, a.created_at
FROM workflow_annotations a
WHERE a.step_id = $1
ORDER BY a.created_at ASC, a.id ASC;
