-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: GetEffectiveActionsPolicyForRepo :one
SELECT
    r.id AS repo_id,
    COALESCE(sp.actions_enabled, true)::boolean AS site_actions_enabled,
    COALESCE(op.actions_enabled, 'inherit'::actions_policy_state)::actions_policy_state AS org_actions_enabled,
    COALESCE(rp.actions_enabled, 'inherit'::actions_policy_state)::actions_policy_state AS repo_actions_enabled,
    CASE
        WHEN COALESCE(sp.actions_enabled, true) = false THEN false
        WHEN COALESCE(rp.actions_enabled, 'inherit'::actions_policy_state) = 'enabled' THEN true
        WHEN COALESCE(rp.actions_enabled, 'inherit'::actions_policy_state) = 'disabled' THEN false
        WHEN COALESCE(op.actions_enabled, 'inherit'::actions_policy_state) = 'enabled' THEN true
        WHEN COALESCE(op.actions_enabled, 'inherit'::actions_policy_state) = 'disabled' THEN false
        ELSE COALESCE(sp.actions_enabled, true)
    END::boolean AS actions_enabled,
    COALESCE(rp.require_pr_approval, op.require_pr_approval, sp.require_pr_approval, true)::boolean AS require_pr_approval,
    COALESCE(rp.max_repo_queued_runs, op.max_repo_queued_runs, sp.max_repo_queued_runs, 50)::integer AS max_repo_queued_runs,
    COALESCE(rp.max_repo_concurrent_jobs, op.max_repo_concurrent_jobs, sp.max_repo_concurrent_jobs, 20)::integer AS max_repo_concurrent_jobs,
    COALESCE(rp.max_owner_concurrent_jobs, op.max_owner_concurrent_jobs, sp.max_owner_concurrent_jobs, 100)::integer AS max_owner_concurrent_jobs,
    COALESCE(rp.actor_trigger_limit_per_hour, op.actor_trigger_limit_per_hour, sp.actor_trigger_limit_per_hour, 120)::integer AS actor_trigger_limit_per_hour
FROM repos r
LEFT JOIN actions_site_policy sp ON sp.id = true
LEFT JOIN actions_org_policies op ON op.org_id = r.owner_org_id
LEFT JOIN actions_repo_policies rp ON rp.repo_id = r.id
WHERE r.id = $1;

-- name: GetActionsRepoPolicy :one
SELECT repo_id, actions_enabled, require_pr_approval,
       max_repo_queued_runs, max_repo_concurrent_jobs,
       max_owner_concurrent_jobs, actor_trigger_limit_per_hour,
       updated_by_user_id, created_at, updated_at
FROM actions_repo_policies
WHERE repo_id = $1;

-- name: UpsertActionsRepoPolicy :one
INSERT INTO actions_repo_policies (
    repo_id, actions_enabled, require_pr_approval,
    max_repo_queued_runs, max_repo_concurrent_jobs,
    max_owner_concurrent_jobs, actor_trigger_limit_per_hour,
    updated_by_user_id
) VALUES (
    $1, $2, sqlc.narg(require_pr_approval)::boolean,
    sqlc.narg(max_repo_queued_runs)::integer,
    sqlc.narg(max_repo_concurrent_jobs)::integer,
    sqlc.narg(max_owner_concurrent_jobs)::integer,
    sqlc.narg(actor_trigger_limit_per_hour)::integer,
    sqlc.narg(updated_by_user_id)::bigint
)
ON CONFLICT (repo_id) DO UPDATE SET
    actions_enabled = EXCLUDED.actions_enabled,
    require_pr_approval = EXCLUDED.require_pr_approval,
    max_repo_queued_runs = EXCLUDED.max_repo_queued_runs,
    max_repo_concurrent_jobs = EXCLUDED.max_repo_concurrent_jobs,
    max_owner_concurrent_jobs = EXCLUDED.max_owner_concurrent_jobs,
    actor_trigger_limit_per_hour = EXCLUDED.actor_trigger_limit_per_hour,
    updated_by_user_id = EXCLUDED.updated_by_user_id,
    updated_at = now()
RETURNING repo_id, actions_enabled, require_pr_approval,
          max_repo_queued_runs, max_repo_concurrent_jobs,
          max_owner_concurrent_jobs, actor_trigger_limit_per_hour,
          updated_by_user_id, created_at, updated_at;

-- name: CountQueuedWorkflowRunsForRepo :one
SELECT COUNT(*)::bigint
FROM workflow_runs
WHERE repo_id = $1 AND status = 'queued';

-- name: CountRecentWorkflowRunsForActor :one
SELECT COUNT(*)::bigint
FROM workflow_runs
WHERE actor_user_id = $1 AND created_at >= sqlc.arg(since)::timestamptz;

-- name: CountRunningWorkflowJobsForRepo :one
SELECT COUNT(*)::bigint
FROM workflow_jobs j
JOIN workflow_runs r ON r.id = j.run_id
WHERE r.repo_id = $1 AND j.status = 'running';

-- name: CountRunningWorkflowJobsForOwner :one
SELECT COUNT(*)::bigint
FROM workflow_jobs j
JOIN workflow_runs wr ON wr.id = j.run_id
JOIN repos run_repo ON run_repo.id = wr.repo_id
JOIN repos anchor_repo ON anchor_repo.id = sqlc.arg(repo_id)::bigint
WHERE j.status = 'running'
  AND (
      (anchor_repo.owner_user_id IS NOT NULL AND run_repo.owner_user_id = anchor_repo.owner_user_id)
      OR (anchor_repo.owner_org_id IS NOT NULL AND run_repo.owner_org_id = anchor_repo.owner_org_id)
  );
