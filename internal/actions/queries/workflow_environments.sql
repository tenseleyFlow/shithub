-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertRepoEnvironment :one
INSERT INTO repo_environments (
    repo_id, name, required_reviewers_enabled, prevent_self_review,
    wait_timer_minutes, deployment_branch_policy
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (repo_id, name) DO UPDATE
SET required_reviewers_enabled = EXCLUDED.required_reviewers_enabled,
    prevent_self_review        = EXCLUDED.prevent_self_review,
    wait_timer_minutes         = EXCLUDED.wait_timer_minutes,
    deployment_branch_policy   = EXCLUDED.deployment_branch_policy,
    updated_at                 = now()
RETURNING id, repo_id, name, required_reviewers_enabled, prevent_self_review,
          wait_timer_minutes, deployment_branch_policy, created_at, updated_at;

-- name: GetRepoEnvironmentByName :one
SELECT id, repo_id, name, required_reviewers_enabled, prevent_self_review,
       wait_timer_minutes, deployment_branch_policy, created_at, updated_at
FROM repo_environments
WHERE repo_id = $1 AND name = $2;

-- name: ListRepoEnvironments :many
SELECT id, repo_id, name, required_reviewers_enabled, prevent_self_review,
       wait_timer_minutes, deployment_branch_policy, created_at, updated_at
FROM repo_environments
WHERE repo_id = $1
ORDER BY name ASC;

-- name: DeleteRepoEnvironment :exec
DELETE FROM repo_environments
WHERE repo_id = $1 AND name = $2;

-- name: ReplaceRepoEnvironmentDeploymentBranches :exec
DELETE FROM repo_environment_deployment_branches
WHERE environment_id = $1;

-- name: InsertRepoEnvironmentDeploymentBranch :one
INSERT INTO repo_environment_deployment_branches (environment_id, pattern)
VALUES ($1, $2)
ON CONFLICT (environment_id, pattern) DO NOTHING
RETURNING id, environment_id, pattern, created_at;

-- name: ListRepoEnvironmentDeploymentBranches :many
SELECT id, environment_id, pattern, created_at
FROM repo_environment_deployment_branches
WHERE environment_id = $1
ORDER BY pattern ASC;
