-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertWorkflowJob :one
INSERT INTO workflow_jobs (
    run_id, job_index, job_key, job_name,
    runs_on, needs_jobs, if_expr, timeout_minutes,
    permissions, job_env, environment_name, environment_url
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at,
          environment_name, environment_url;

-- name: GetWorkflowJobByID :one
SELECT id, run_id, job_index, job_key, job_name, runs_on,
       runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
       job_env, status, conclusion, cancel_requested,
       started_at, completed_at, version, created_at, updated_at,
       environment_name, environment_url
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
          started_at, completed_at, version, created_at, updated_at,
          environment_name, environment_url;

-- name: RequestWorkflowJobCancel :one
UPDATE workflow_jobs
SET cancel_requested = true,
    status = CASE
        WHEN status = 'queued' THEN 'cancelled'::workflow_job_status
        ELSE status
    END,
    conclusion = CASE
        WHEN status = 'queued' THEN 'cancelled'::check_conclusion
        ELSE conclusion
    END,
    started_at = CASE
        WHEN status = 'queued' THEN COALESCE(started_at, now())
        ELSE started_at
    END,
    completed_at = CASE
        WHEN status = 'queued' THEN COALESCE(completed_at, now())
        ELSE completed_at
    END,
    version = version + 1,
    updated_at = now()
WHERE id = $1
  AND status IN ('queued', 'running')
  AND (status = 'queued' OR cancel_requested = false)
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at,
          environment_name, environment_url;

-- name: RequestWorkflowRunCancel :many
UPDATE workflow_jobs
SET cancel_requested = true,
    status = CASE
        WHEN status = 'queued' THEN 'cancelled'::workflow_job_status
        ELSE status
    END,
    conclusion = CASE
        WHEN status = 'queued' THEN 'cancelled'::check_conclusion
        ELSE conclusion
    END,
    started_at = CASE
        WHEN status = 'queued' THEN COALESCE(started_at, now())
        ELSE started_at
    END,
    completed_at = CASE
        WHEN status = 'queued' THEN COALESCE(completed_at, now())
        ELSE completed_at
    END,
    version = version + 1,
    updated_at = now()
WHERE run_id = $1
  AND status IN ('queued', 'running')
  AND (status = 'queued' OR cancel_requested = false)
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at,
          environment_name, environment_url;

-- name: CountRunningJobsForRunner :one
SELECT COUNT(*)::integer
FROM workflow_jobs
WHERE runner_id = sqlc.arg(runner_id)::bigint AND status = 'running';

-- name: ListRunnerRunningJobsForAdmin :many
SELECT j.id,
       j.run_id,
       j.job_key,
       j.job_name,
       j.runs_on,
       j.runner_id,
       j.status,
       j.conclusion,
       j.cancel_requested,
       j.started_at,
       j.completed_at,
       j.created_at,
       run.repo_id,
       run.run_index,
       run.workflow_file,
       run.workflow_name,
       run.head_ref,
       run.head_sha,
       repo.name AS repo_name,
       COALESCE(owner_user.username, owner_org.slug, '')::text AS owner_login,
       runner.name AS runner_name,
       runner.status AS runner_status,
       runner.last_heartbeat_at AS runner_last_heartbeat_at
FROM workflow_jobs j
JOIN workflow_runs run ON run.id = j.run_id
JOIN repos repo ON repo.id = run.repo_id
LEFT JOIN users owner_user ON owner_user.id = repo.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = repo.owner_org_id
LEFT JOIN workflow_runners runner ON runner.id = j.runner_id
WHERE j.status = 'running'
  AND j.runner_id IS NOT NULL
  AND (sqlc.arg(runner_id)::bigint = 0 OR j.runner_id = sqlc.arg(runner_id)::bigint)
ORDER BY j.started_at ASC NULLS FIRST, j.id ASC
LIMIT sqlc.arg(limit_count)::integer;

-- name: CancelRunnerJobsMissingFromActiveSet :many
UPDATE workflow_jobs
SET cancel_requested = true,
    status = 'cancelled',
    conclusion = 'cancelled',
    started_at = COALESCE(started_at, now()),
    completed_at = COALESCE(completed_at, now()),
    version = version + 1,
    updated_at = now()
WHERE runner_id = sqlc.arg(runner_id)::bigint
  AND status = 'running'
  AND NOT (id = ANY(sqlc.arg(active_job_ids)::bigint[]))
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at,
          environment_name, environment_url;

-- name: ClaimQueuedWorkflowJob :one
WITH candidate AS (
    SELECT j.id
    FROM workflow_jobs j
    JOIN workflow_runs r ON r.id = j.run_id
    JOIN repos repo ON repo.id = r.repo_id
    LEFT JOIN actions_site_policy sp ON sp.id = true
    LEFT JOIN actions_org_policies op ON op.org_id = repo.owner_org_id
    LEFT JOIN actions_repo_policies rp ON rp.repo_id = r.repo_id
    CROSS JOIN LATERAL (
        SELECT
            CASE
                WHEN r.head_ref LIKE 'refs/heads/%' THEN 'branch'
                WHEN r.head_ref LIKE 'refs/tags/%' THEN 'tag'
                ELSE ''
            END::text AS target,
            CASE
                WHEN r.head_ref LIKE 'refs/heads/%' THEN substring(r.head_ref FROM 12)
                WHEN r.head_ref LIKE 'refs/tags/%' THEN substring(r.head_ref FROM 11)
                ELSE ''
            END::text AS name
    ) deployment_ref
    WHERE j.status = 'queued'
      AND r.status IN ('queued', 'running')
      AND (r.need_approval = false OR r.approved_by_user_id IS NOT NULL)
      AND j.cancel_requested = false
      AND j.runner_id IS NULL
      AND CASE
          WHEN COALESCE(sp.actions_enabled, true) = false THEN false
          WHEN COALESCE(rp.actions_enabled, 'inherit'::actions_policy_state) = 'enabled' THEN true
          WHEN COALESCE(rp.actions_enabled, 'inherit'::actions_policy_state) = 'disabled' THEN false
          WHEN COALESCE(op.actions_enabled, 'inherit'::actions_policy_state) = 'enabled' THEN true
          WHEN COALESCE(op.actions_enabled, 'inherit'::actions_policy_state) = 'disabled' THEN false
          ELSE COALESCE(sp.actions_enabled, true)
      END
      AND (
          SELECT COUNT(*)::integer
          FROM workflow_jobs running_job
          JOIN workflow_runs running_run ON running_run.id = running_job.run_id
          WHERE running_job.status = 'running'
            AND running_run.repo_id = r.repo_id
      ) < COALESCE(rp.max_repo_concurrent_jobs, op.max_repo_concurrent_jobs, sp.max_repo_concurrent_jobs, 20)
      AND (
          SELECT COUNT(*)::integer
          FROM workflow_jobs running_job
          JOIN workflow_runs running_run ON running_run.id = running_job.run_id
          JOIN repos running_repo ON running_repo.id = running_run.repo_id
          WHERE running_job.status = 'running'
            AND (
                (repo.owner_user_id IS NOT NULL AND running_repo.owner_user_id = repo.owner_user_id)
                OR (repo.owner_org_id IS NOT NULL AND running_repo.owner_org_id = repo.owner_org_id)
            )
      ) < COALESCE(rp.max_owner_concurrent_jobs, op.max_owner_concurrent_jobs, sp.max_owner_concurrent_jobs, 100)
      AND (j.runs_on = '' OR j.runs_on = ANY(sqlc.arg(labels)::text[]))
      AND NOT EXISTS (
          SELECT 1
          FROM workflow_jobs dep
          WHERE dep.run_id = j.run_id
            AND dep.job_key = ANY(j.needs_jobs)
            AND (dep.status <> 'completed' OR dep.conclusion <> 'success')
      )
      AND NOT EXISTS (
          SELECT 1
          FROM workflow_runs blocker
          WHERE r.concurrency_group <> ''
            AND blocker.repo_id = r.repo_id
            AND blocker.concurrency_group = r.concurrency_group
            AND blocker.id <> r.id
            AND blocker.status IN ('queued', 'running')
            AND (blocker.created_at, blocker.id) < (r.created_at, r.id)
            AND EXISTS (
                SELECT 1
                FROM workflow_jobs blocker_job
                WHERE blocker_job.run_id = blocker.id
                  AND blocker_job.status IN ('queued', 'running')
                  AND blocker_job.cancel_requested = false
            )
      )
      AND NOT EXISTS (
          SELECT 1
          FROM repo_environments env
          WHERE env.repo_id = r.repo_id
            AND env.name = j.environment_name
            AND NOT (
                env.deployment_branch_policy = 'all'
                OR (
                    env.deployment_branch_policy = 'protected'
                    AND deployment_ref.target <> ''
                    AND (
                        NOT EXISTS (
                            SELECT 1
                            FROM branch_protection_rules bpr_any
                            WHERE bpr_any.repo_id = r.repo_id
                              AND bpr_any.target = deployment_ref.target
                        )
                        OR EXISTS (
                            SELECT 1
                            FROM branch_protection_rules bpr
                            WHERE bpr.repo_id = r.repo_id
                              AND bpr.target = deployment_ref.target
                              AND shithub_deployment_pattern_matches(bpr.pattern, deployment_ref.name)
                        )
                    )
                )
                OR (
                    env.deployment_branch_policy = 'selected'
                    AND deployment_ref.target <> ''
                    AND EXISTS (
                        SELECT 1
                        FROM repo_environment_deployment_branches edb
                        WHERE edb.environment_id = env.id
                          AND shithub_deployment_pattern_matches(edb.pattern, deployment_ref.name)
                    )
                )
            )
      )
      AND NOT EXISTS (
          SELECT 1
          FROM repo_environments env
          WHERE env.repo_id = r.repo_id
            AND env.name = j.environment_name
            AND env.wait_timer_minutes > 0
            AND j.created_at + make_interval(mins => env.wait_timer_minutes) > now()
      )
      AND NOT EXISTS (
          SELECT 1
          FROM repo_environments env
          WHERE env.repo_id = r.repo_id
            AND env.name = j.environment_name
            AND env.required_reviewers_enabled = true
            AND r.need_approval = true
            AND r.approved_by_user_id IS NULL
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
              j.permissions, j.job_env, j.environment_name, j.environment_url,
              j.status, j.conclusion,
              j.cancel_requested, j.started_at, j.completed_at, j.version,
              j.created_at, j.updated_at
)
SELECT c.id, c.run_id, c.job_index, c.job_key, c.job_name, c.runs_on,
       c.runner_id, c.needs_jobs, c.if_expr, c.timeout_minutes,
       c.permissions, c.job_env, c.environment_name, c.environment_url,
       c.status, c.conclusion,
       c.cancel_requested, c.started_at, c.completed_at, c.version,
       c.created_at, c.updated_at,
       r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
       r.head_sha, r.head_ref, r.event, r.event_payload,
       COALESCE(owner_user.username, owner_org.slug)::text AS repo_owner,
       repo.name AS repo_name
FROM claimed c
JOIN workflow_runs r ON r.id = c.run_id
JOIN repos repo ON repo.id = r.repo_id
LEFT JOIN users owner_user ON owner_user.id = repo.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = repo.owner_org_id;

-- name: ListJobsForRun :many
SELECT id, run_id, job_index, job_key, job_name, runs_on, status,
       conclusion, cancel_requested, needs_jobs, environment_name, environment_url,
       started_at, completed_at, created_at, updated_at
FROM workflow_jobs
WHERE run_id = $1
ORDER BY job_index ASC;

-- name: ListQueuedWorkflowJobRunsOn :many
SELECT
    COALESCE(NULLIF(j.runs_on, ''), '(none)')::text AS runs_on,
    COUNT(DISTINCT j.id)::integer AS queued_jobs,
    COUNT(DISTINCT wr.id)::integer AS matching_runner_count,
    MIN(j.created_at)::timestamptz AS oldest_queued_at
FROM workflow_jobs j
LEFT JOIN workflow_runners wr
  ON (j.runs_on = '' OR j.runs_on = ANY(wr.labels))
 AND wr.status IN ('idle', 'busy')
 AND wr.draining_at IS NULL
 AND wr.revoked_at IS NULL
WHERE j.status = 'queued'
  AND j.cancel_requested = false
  AND j.runner_id IS NULL
GROUP BY COALESCE(NULLIF(j.runs_on, ''), '(none)')
ORDER BY queued_jobs DESC, runs_on ASC;
