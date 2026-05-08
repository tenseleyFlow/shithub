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
          has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method;

-- name: GetRepoByID :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method
FROM repos
WHERE id = $1;

-- name: GetRepoOwnerUsernameByID :one
-- Returns the owner_username for a repo. Used by size-recalc and other
-- jobs that need to derive the bare-repo on-disk path without round-
-- tripping through the full user row.
SELECT u.username AS owner_username, r.name AS repo_name
FROM repos r
JOIN users u ON u.id = r.owner_user_id
WHERE r.id = $1;

-- name: GetRepoByOwnerUserAndName :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method
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
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method
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

-- name: UpdateRepoDefaultBranchOID :exec
-- Set when push:process detects a commit on the repo's default branch.
-- Pass NULL to clear (e.g. when the branch is force-deleted in a future
-- sprint). The repo home view reads this to decide between empty and
-- populated layouts.
UPDATE repos SET default_branch_oid = sqlc.narg(default_branch_oid)::text WHERE id = $1;

-- name: ListAllRepoFullNames :many
-- Used by `shithubd hooks reinstall --all` to enumerate every active
-- bare repo on disk and re-link its hooks.
SELECT
    r.id,
    r.name,
    u.username AS owner_username
FROM repos r
JOIN users u ON u.id = r.owner_user_id
WHERE r.deleted_at IS NULL
ORDER BY r.id;
