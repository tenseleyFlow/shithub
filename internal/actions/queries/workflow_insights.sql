-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: GetActionsUsageSummaryForRepo :one
SELECT
    COUNT(DISTINCT run.id)::bigint AS run_count,
    COUNT(job.id)::bigint AS job_count,
    COALESCE(SUM(EXTRACT(EPOCH FROM (job.completed_at - job.started_at))) FILTER (
        WHERE job.started_at IS NOT NULL AND job.completed_at IS NOT NULL
    ), 0)::bigint AS completed_job_seconds
FROM workflow_runs run
LEFT JOIN workflow_jobs job ON job.run_id = run.id
WHERE run.repo_id = $1
  AND run.created_at >= $2;

-- name: ListActionsUsageWorkflowsForRepo :many
SELECT
    run.workflow_file,
    COALESCE(NULLIF(run.workflow_name, ''), run.workflow_file)::text AS workflow_name,
    COUNT(DISTINCT run.id)::bigint AS run_count,
    COUNT(job.id)::bigint AS job_count,
    COALESCE(SUM(EXTRACT(EPOCH FROM (job.completed_at - job.started_at))) FILTER (
        WHERE job.started_at IS NOT NULL AND job.completed_at IS NOT NULL
    ), 0)::bigint AS completed_job_seconds
FROM workflow_runs run
LEFT JOIN workflow_jobs job ON job.run_id = run.id
WHERE run.repo_id = $1
  AND run.created_at >= $2
GROUP BY run.workflow_file, COALESCE(NULLIF(run.workflow_name, ''), run.workflow_file)
ORDER BY completed_job_seconds DESC, run_count DESC, lower(COALESCE(NULLIF(run.workflow_name, ''), run.workflow_file)), run.workflow_file
LIMIT $3;

-- name: GetActionsPerformanceSummaryForRepo :one
SELECT
    COALESCE(AVG(EXTRACT(EPOCH FROM (job.completed_at - job.started_at))) FILTER (
        WHERE job.started_at IS NOT NULL AND job.completed_at IS NOT NULL
    ), 0)::double precision AS avg_job_seconds,
    COALESCE(AVG(EXTRACT(EPOCH FROM (job.started_at - job.created_at))) FILTER (
        WHERE job.started_at IS NOT NULL AND job.started_at >= job.created_at
    ), 0)::double precision AS avg_queue_seconds,
    COUNT(job.id) FILTER (
        WHERE job.status IN ('completed', 'cancelled', 'skipped')
    )::bigint AS terminal_job_count,
    COUNT(job.id) FILTER (
        WHERE job.status IN ('completed', 'cancelled')
          AND job.conclusion IS NOT NULL
          AND job.conclusion <> 'success'
    )::bigint AS failed_job_count,
    COALESCE(SUM(EXTRACT(EPOCH FROM (job.completed_at - job.started_at))) FILTER (
        WHERE job.started_at IS NOT NULL
          AND job.completed_at IS NOT NULL
          AND job.conclusion IS NOT NULL
          AND job.conclusion <> 'success'
    ), 0)::bigint AS failed_job_seconds
FROM workflow_runs run
JOIN workflow_jobs job ON job.run_id = run.id
WHERE run.repo_id = $1
  AND run.created_at >= $2;

-- name: ListActionsPerformanceWorkflowsForRepo :many
SELECT
    run.workflow_file,
    COALESCE(NULLIF(run.workflow_name, ''), run.workflow_file)::text AS workflow_name,
    COUNT(DISTINCT run.id)::bigint AS run_count,
    COUNT(job.id)::bigint AS job_count,
    COALESCE(AVG(EXTRACT(EPOCH FROM (job.completed_at - job.started_at))) FILTER (
        WHERE job.started_at IS NOT NULL AND job.completed_at IS NOT NULL
    ), 0)::double precision AS avg_job_seconds,
    COALESCE(AVG(EXTRACT(EPOCH FROM (job.started_at - job.created_at))) FILTER (
        WHERE job.started_at IS NOT NULL AND job.started_at >= job.created_at
    ), 0)::double precision AS avg_queue_seconds,
    COUNT(job.id) FILTER (
        WHERE job.status IN ('completed', 'cancelled', 'skipped')
    )::bigint AS terminal_job_count,
    COUNT(job.id) FILTER (
        WHERE job.status IN ('completed', 'cancelled')
          AND job.conclusion IS NOT NULL
          AND job.conclusion <> 'success'
    )::bigint AS failed_job_count
FROM workflow_runs run
JOIN workflow_jobs job ON job.run_id = run.id
WHERE run.repo_id = $1
  AND run.created_at >= $2
GROUP BY run.workflow_file, COALESCE(NULLIF(run.workflow_name, ''), run.workflow_file)
ORDER BY failed_job_count DESC, avg_job_seconds DESC, run_count DESC, lower(COALESCE(NULLIF(run.workflow_name, ''), run.workflow_file)), run.workflow_file
LIMIT $3;
