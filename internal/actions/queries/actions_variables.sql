-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertRepoVariable :one
INSERT INTO actions_variables (repo_id, name, value, created_by_user_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (repo_id, name) WHERE repo_id IS NOT NULL DO UPDATE
SET value = EXCLUDED.value, updated_at = now()
RETURNING id, repo_id, org_id, name, value, created_by_user_id,
          created_at, updated_at;

-- name: UpsertOrgVariable :one
INSERT INTO actions_variables (org_id, name, value, created_by_user_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (org_id, name) WHERE org_id IS NOT NULL DO UPDATE
SET value = EXCLUDED.value, updated_at = now()
RETURNING id, repo_id, org_id, name, value, created_by_user_id,
          created_at, updated_at;

-- name: ListRepoVariables :many
SELECT id, name, value, created_at, updated_at
FROM actions_variables
WHERE repo_id = $1
ORDER BY name ASC;

-- name: ListOrgVariables :many
SELECT id, name, value, created_at, updated_at
FROM actions_variables
WHERE org_id = $1
ORDER BY name ASC;

-- name: DeleteRepoVariable :exec
DELETE FROM actions_variables WHERE repo_id = $1 AND name = $2;

-- name: DeleteOrgVariable :exec
DELETE FROM actions_variables WHERE org_id = $1 AND name = $2;
