-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25b dependency update automation state.

-- name: UpsertDependencyUpdateConfig :one
INSERT INTO dependency_update_configs (
    repo_id, ecosystem, package_manager, directory,
    schedule_interval, schedule_day, schedule_time, schedule_timezone, schedule_cron,
    open_pull_request_limit, target_branch, allow_rules, ignore_rules,
    groups, registries, unsupported_keys, enabled, raw_config_hash,
    raw_config_path, last_synced_sha, next_run_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8, $9,
    $10, $11, sqlc.arg(allow_rules)::jsonb, sqlc.arg(ignore_rules)::jsonb,
    sqlc.arg(groups)::jsonb, sqlc.arg(registries)::jsonb, sqlc.arg(unsupported_keys)::text[],
    $12, $13, $14, $15, sqlc.narg(next_run_at)::timestamptz
)
ON CONFLICT (repo_id, ecosystem, directory) DO UPDATE
SET package_manager = EXCLUDED.package_manager,
    schedule_interval = EXCLUDED.schedule_interval,
    schedule_day = EXCLUDED.schedule_day,
    schedule_time = EXCLUDED.schedule_time,
    schedule_timezone = EXCLUDED.schedule_timezone,
    schedule_cron = EXCLUDED.schedule_cron,
    open_pull_request_limit = EXCLUDED.open_pull_request_limit,
    target_branch = EXCLUDED.target_branch,
    allow_rules = EXCLUDED.allow_rules,
    ignore_rules = EXCLUDED.ignore_rules,
    groups = EXCLUDED.groups,
    registries = EXCLUDED.registries,
    unsupported_keys = EXCLUDED.unsupported_keys,
    enabled = EXCLUDED.enabled,
    raw_config_hash = EXCLUDED.raw_config_hash,
    raw_config_path = EXCLUDED.raw_config_path,
    last_synced_sha = EXCLUDED.last_synced_sha,
    next_run_at = EXCLUDED.next_run_at
RETURNING *;

-- name: GetDependencyUpdateConfig :one
SELECT *
FROM dependency_update_configs
WHERE id = $1;

-- name: ListDependencyUpdateConfigsForRepo :many
SELECT *
FROM dependency_update_configs
WHERE repo_id = $1
ORDER BY ecosystem, directory, id;

-- name: ListEnabledDependencyUpdateConfigsForRepo :many
SELECT *
FROM dependency_update_configs
WHERE repo_id = $1
  AND enabled = true
ORDER BY ecosystem, directory, id;

-- name: ListDueDependencyUpdateConfigs :many
SELECT *
FROM dependency_update_configs
WHERE enabled = true
  AND next_run_at IS NOT NULL
  AND next_run_at <= now()
ORDER BY next_run_at ASC, id ASC
LIMIT sqlc.arg(limit_rows)::int;

-- name: ClaimDueDependencyUpdateConfigs :many
WITH due AS (
    SELECT id
    FROM dependency_update_configs
    WHERE enabled = true
      AND next_run_at IS NOT NULL
      AND next_run_at <= sqlc.arg(now_at)::timestamptz
    ORDER BY next_run_at ASC, id ASC
    LIMIT sqlc.arg(limit_rows)::int
    FOR UPDATE SKIP LOCKED
)
SELECT dependency_update_configs.*
FROM dependency_update_configs
JOIN due ON due.id = dependency_update_configs.id
ORDER BY dependency_update_configs.next_run_at ASC, dependency_update_configs.id ASC;

-- name: DisableMissingDependencyUpdateConfigs :exec
UPDATE dependency_update_configs
SET enabled = false,
    next_run_at = NULL
WHERE repo_id = sqlc.arg(repo_id)::bigint
  AND NOT (id = ANY(sqlc.arg(active_ids)::bigint[]));

-- name: TouchDependencyUpdateConfigChecked :one
UPDATE dependency_update_configs
SET last_checked_at = now(),
    next_run_at = sqlc.narg(next_run_at)::timestamptz
WHERE id = $1
RETURNING *;

-- name: CreateDependencyUpdateJob :one
INSERT INTO dependency_update_jobs (
    repo_id, config_id, job_kind, status, trigger_source,
    scheduled_for, base_sha, head_sha, result_summary, last_error
) VALUES (
    $1, sqlc.narg(config_id)::bigint, $2, $3, $4,
    sqlc.narg(scheduled_for)::timestamptz, $5, $6, sqlc.arg(result_summary)::jsonb,
    sqlc.arg(last_error)::text
)
RETURNING *;

-- name: GetDependencyUpdateJob :one
SELECT *
FROM dependency_update_jobs
WHERE id = $1;

-- name: MarkDependencyUpdateJobRunning :one
UPDATE dependency_update_jobs
SET status = 'running',
    started_at = COALESCE(started_at, now()),
    last_error = ''
WHERE id = $1
RETURNING *;

-- name: MarkQueuedDependencyUpdateJobRunning :one
UPDATE dependency_update_jobs
SET status = 'running',
    started_at = COALESCE(started_at, now()),
    completed_at = NULL,
    last_error = ''
WHERE id = $1
  AND status = 'queued'
RETURNING *;

-- name: CompleteDependencyUpdateJob :one
UPDATE dependency_update_jobs
SET status = sqlc.arg(status)::text,
    completed_at = COALESCE(completed_at, now()),
    base_sha = sqlc.arg(base_sha)::text,
    head_sha = sqlc.arg(head_sha)::text,
    result_summary = sqlc.arg(result_summary)::jsonb,
    last_error = sqlc.arg(last_error)::text
WHERE id = sqlc.arg(id)::bigint
RETURNING *;

-- name: ListDependencyUpdateJobsForRepo :many
SELECT *
FROM dependency_update_jobs
WHERE repo_id = $1
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(limit_rows)::int;

-- name: CountActiveDependencyUpdateJobsForConfigKind :one
SELECT count(*)::bigint
FROM dependency_update_jobs
WHERE config_id = $1
  AND job_kind = $2
  AND status IN ('queued', 'running');

-- name: UpsertDependencyUpdatePR :one
INSERT INTO dependency_update_prs (
    job_id, repo_id, pull_request_id, branch_name,
    package_set, update_kind, status
) VALUES (
    sqlc.narg(job_id)::bigint, $1, sqlc.narg(pull_request_id)::bigint, $2,
    sqlc.arg(package_set)::jsonb, $3, $4
)
ON CONFLICT (repo_id, branch_name) DO UPDATE
SET job_id = EXCLUDED.job_id,
    pull_request_id = EXCLUDED.pull_request_id,
    package_set = EXCLUDED.package_set,
    update_kind = EXCLUDED.update_kind,
    status = EXCLUDED.status
RETURNING *;

-- name: ListDependencyUpdatePRsForRepo :many
SELECT *
FROM dependency_update_prs
WHERE repo_id = $1
ORDER BY updated_at DESC, id DESC;

-- name: CreateDependencyAutoTriageRule :one
INSERT INTO dependency_auto_triage_rules (
    org_id, repo_id, name, enabled, priority,
    match_conditions, actions, created_by
) VALUES (
    sqlc.narg(org_id)::bigint, sqlc.narg(repo_id)::bigint, $1, $2, $3,
    sqlc.arg(match_conditions)::jsonb, sqlc.arg(actions)::jsonb,
    sqlc.narg(created_by)::bigint
)
RETURNING *;

-- name: UpdateDependencyAutoTriageRule :one
UPDATE dependency_auto_triage_rules
SET name = $2,
    enabled = $3,
    priority = $4,
    match_conditions = sqlc.arg(match_conditions)::jsonb,
    actions = sqlc.arg(actions)::jsonb
WHERE id = $1
RETURNING *;

-- name: ListDependencyAutoTriageRulesForRepo :many
SELECT rule.*
FROM dependency_auto_triage_rules rule
LEFT JOIN repos repo ON repo.id = sqlc.arg(repo_id)::bigint
WHERE rule.enabled = true
  AND (
      rule.repo_id = sqlc.arg(repo_id)::bigint
      OR (repo.owner_org_id IS NOT NULL AND rule.org_id = repo.owner_org_id)
  )
ORDER BY rule.priority ASC, rule.id ASC;

-- name: RecordDependencyAutoTriageEvent :one
INSERT INTO dependency_auto_triage_events (
    rule_id, repo_id, alert_id, action, outcome, message
) VALUES (
    sqlc.narg(rule_id)::bigint, $1, $2, $3, $4, $5
)
RETURNING *;

-- name: ListDependencyAutoTriageEventsForAlert :many
SELECT *
FROM dependency_auto_triage_events
WHERE alert_id = $1
ORDER BY created_at DESC, id DESC;
