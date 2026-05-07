-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateRepo :one
INSERT INTO repos (
    owner_user_id, owner_org_id, name, description, visibility,
    default_branch, license_key, primary_language
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING id, owner_user_id, owner_org_id, name, description, visibility,
          default_branch, is_archived, archived_at, deleted_at,
          disk_used_bytes, fork_of_repo_id, license_key, primary_language,
          has_issues, has_pulls, created_at, updated_at;

-- name: GetRepoByOwnerUserAndName :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at
FROM repos
WHERE owner_user_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: ExistsRepoForOwnerUser :one
SELECT EXISTS(
    SELECT 1 FROM repos
    WHERE owner_user_id = $1 AND name = $2 AND deleted_at IS NULL
);

-- name: ListReposForOwnerUser :many
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at
FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC;

-- name: CountReposForOwnerUser :one
SELECT count(*) FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteRepo :exec
UPDATE repos SET deleted_at = now() WHERE id = $1;

-- name: UpdateRepoDiskUsed :exec
UPDATE repos SET disk_used_bytes = $2 WHERE id = $1;
