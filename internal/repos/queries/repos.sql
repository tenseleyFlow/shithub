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
          allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
          star_count, watcher_count, fork_count, init_status,
          last_indexed_oid;

-- name: GetRepoByID :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid
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
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid
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
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid
FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC;

-- name: CountReposForOwnerUser :one
SELECT count(*) FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL;

-- name: GetRepoByOwnerOrgAndName :one
-- S30: org-owner mirror of GetRepoByOwnerUserAndName. The (owner_org_id,
-- name) partial unique index from 0017 backs this lookup with the same
-- O(1) cost the user-side path enjoys.
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid
FROM repos
WHERE owner_org_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: ExistsRepoForOwnerOrg :one
SELECT EXISTS(
    SELECT 1 FROM repos
    WHERE owner_org_id = $1 AND name = $2 AND deleted_at IS NULL
);

-- name: ListReposForOwnerOrg :many
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid
FROM repos
WHERE owner_org_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC;

-- name: UpdateRepoGeneralSettings :exec
-- S32: General-tab settings persist via this single query so each
-- form post is one round-trip. The merge-method toggles are kept
-- separate from the repo create flow because they're admin-only.
UPDATE repos
   SET description       = $2,
       has_issues        = $3,
       has_pulls         = $4,
       updated_at        = now()
 WHERE id = $1;

-- name: UpdateRepoMergeSettings :exec
UPDATE repos
   SET allow_merge_commit  = $2,
       allow_squash_merge  = $3,
       allow_rebase_merge  = $4,
       default_merge_method = $5,
       updated_at          = now()
 WHERE id = $1;

-- ─── repo_topics (S32) ─────────────────────────────────────────────

-- name: ListRepoTopics :many
SELECT topic FROM repo_topics WHERE repo_id = $1 ORDER BY topic ASC;

-- name: ReplaceRepoTopics :exec
-- Atomic full-replace: callers compose the new topic set in Go,
-- then replace the existing rows in one tx (DELETE + INSERT). The
-- caller's tx wraps both calls for atomicity.
DELETE FROM repo_topics WHERE repo_id = $1;

-- name: InsertRepoTopic :exec
INSERT INTO repo_topics (repo_id, topic)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;

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

-- ─── S27 forks ─────────────────────────────────────────────────────

-- name: CreateForkRepo :one
-- Insert a fork shell. Distinct from CreateRepo because forks set
-- `fork_of_repo_id` (which fires the fork_count trigger) and start
-- at init_status='init_pending' so the worker job can flip them to
-- 'initialized' once `git clone --bare --shared` finishes.
INSERT INTO repos (
    owner_user_id, owner_org_id, name, description, visibility,
    default_branch, fork_of_repo_id, init_status
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, 'init_pending'
)
RETURNING id, owner_user_id, owner_org_id, name, description, visibility,
          default_branch, is_archived, archived_at, deleted_at,
          disk_used_bytes, fork_of_repo_id, license_key, primary_language,
          has_issues, has_pulls, created_at, updated_at, default_branch_oid,
          allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
          star_count, watcher_count, fork_count, init_status,
          last_indexed_oid;

-- name: SetLastIndexedOID :exec
-- S28 code-search: the worker writes the OID it finished indexing
-- so the reconciler can detect drift (default_branch_oid moved but
-- last_indexed_oid lagged).
UPDATE repos SET last_indexed_oid = sqlc.narg(last_indexed_oid)::text WHERE id = $1;

-- name: ListReposNeedingReindex :many
-- S28 code-search reconciler: returns repos whose default_branch_oid
-- has advanced past last_indexed_oid (or last_indexed_oid is NULL
-- and a default exists). Limited so a single tick of the cron
-- doesn't try to re-index the whole world.
SELECT id, name, default_branch, default_branch_oid
FROM repos
WHERE deleted_at IS NULL
  AND default_branch_oid IS NOT NULL
  AND (last_indexed_oid IS NULL OR last_indexed_oid <> default_branch_oid)
ORDER BY id
LIMIT $1;

-- name: SetRepoInitStatus :exec
-- Promotes a fork from init_pending to initialized (or init_failed).
-- The DB row is created up-front so the URL resolves immediately and
-- the user sees a "preparing your fork" placeholder while the worker
-- runs `git clone --bare --shared`.
UPDATE repos SET init_status = $2 WHERE id = $1;

-- name: ListForksOfRepo :many
-- Forks of a given source repo, paginated, recency-sorted. Joined
-- with users for the owner display name. Excludes soft-deleted.
SELECT r.id, r.name, r.description, r.visibility, r.created_at,
       r.star_count, r.fork_count, r.init_status,
       u.username AS owner_username, u.display_name AS owner_display_name
FROM repos r
JOIN users u ON u.id = r.owner_user_id
WHERE r.fork_of_repo_id = $1
  AND r.deleted_at IS NULL
ORDER BY r.created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountForksOfRepo :one
SELECT count(*) FROM repos
WHERE fork_of_repo_id = $1 AND deleted_at IS NULL;

-- name: ListForksOfRepoForRepack :many
-- Used by S16's hard-delete cascade (S27 amendment): before deleting
-- a source repo, every fork must `git repack -a -d --no-shared` so
-- it has its own copy of the objects. Returns just enough to locate
-- the bare repo on disk.
SELECT r.id, r.name, u.username AS owner_username
FROM repos r
JOIN users u ON u.id = r.owner_user_id
WHERE r.fork_of_repo_id = $1
  AND r.deleted_at IS NULL;
