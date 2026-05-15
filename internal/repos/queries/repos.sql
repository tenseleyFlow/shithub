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
          last_indexed_oid, is_template;

-- name: LockRepoOwnerName :exec
-- Serializes DB + filesystem operations for one logical owner/name
-- pair. Create, soft-delete, restore, and hard-delete all touch the
-- canonical bare path; a transaction-scoped advisory lock keeps those
-- cross-resource moves from racing.
SELECT pg_advisory_xact_lock(hashtextextended($1, 0));

-- name: GetRepoByID :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE id = $1;

-- name: GetRepoOwnerUsernameByID :one
-- Returns the owner slug for a repo. Used by size-recalc, indexing, and
-- other jobs that need the bare-repo on-disk path. Org-owned repos use the
-- org slug in the same path position as user-owned repos.
SELECT COALESCE(u.username::varchar, o.slug::varchar) AS owner_username, r.name AS repo_name
FROM repos r
LEFT JOIN users u ON u.id = r.owner_user_id
LEFT JOIN orgs o ON o.id = r.owner_org_id
WHERE r.id = $1;

-- name: GetRepoByOwnerUserAndName :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_user_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: GetSoftDeletedRepoByOwnerUserAndName :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_user_id = $1 AND name = $2 AND deleted_at IS NOT NULL
ORDER BY deleted_at DESC, id DESC
LIMIT 1;

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
       last_indexed_oid, is_template
FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC;

-- name: CountReposForOwnerUser :one
SELECT count(*) FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL;

-- name: ListReposForOwnerUserPaged :many
-- Paginated mirror of ListReposForOwnerUser. Used by the REST surface
-- where the caller is the owner (or a site admin) and is allowed to see
-- private repos. Order matches the un-paginated form so callers can
-- swap between them without observing a reshuffle.
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC
LIMIT $2 OFFSET $3;

-- name: ListPublicReposForOwnerUser :many
-- Public-only view of a user's repos for the "list another user's repos"
-- REST endpoint. Hidden behind the same updated_at ordering.
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_user_id = $1
  AND visibility = 'public'
  AND deleted_at IS NULL
ORDER BY updated_at DESC
LIMIT $2 OFFSET $3;

-- name: CountPublicReposForOwnerUser :one
SELECT count(*) FROM repos
WHERE owner_user_id = $1
  AND visibility = 'public'
  AND deleted_at IS NULL;

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
       last_indexed_oid, is_template
FROM repos
WHERE owner_org_id = $1 AND name = $2 AND deleted_at IS NULL;

-- name: GetSoftDeletedRepoByOwnerOrgAndName :one
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_org_id = $1 AND name = $2 AND deleted_at IS NOT NULL
ORDER BY deleted_at DESC, id DESC
LIMIT 1;

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
       last_indexed_oid, is_template
FROM repos
WHERE owner_org_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC;

-- name: ListReposForOwnerOrgPaged :many
-- Paginated mirror of ListReposForOwnerOrg for the REST list endpoint
-- when the viewer is an org member and may see private repos.
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_org_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC
LIMIT $2 OFFSET $3;

-- name: CountReposForOwnerOrg :one
SELECT count(*) FROM repos
WHERE owner_org_id = $1 AND deleted_at IS NULL;

-- name: ListPublicReposForOwnerOrg :many
-- Public-only org repo listing for non-member callers (and the no-auth
-- public-discovery REST view).
SELECT id, owner_user_id, owner_org_id, name, description, visibility,
       default_branch, is_archived, archived_at, deleted_at,
       disk_used_bytes, fork_of_repo_id, license_key, primary_language,
       has_issues, has_pulls, created_at, updated_at, default_branch_oid,
       allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
       star_count, watcher_count, fork_count, init_status,
       last_indexed_oid, is_template
FROM repos
WHERE owner_org_id = $1
  AND visibility = 'public'
  AND deleted_at IS NULL
ORDER BY updated_at DESC
LIMIT $2 OFFSET $3;

-- name: CountPublicReposForOwnerOrg :one
SELECT count(*) FROM repos
WHERE owner_org_id = $1
  AND visibility = 'public'
  AND deleted_at IS NULL;

-- name: ListProfilePinCandidateReposForUser :many
SELECT sqlc.embed(r), COALESCE(owner_user.username, owner_org.slug)::text AS owner_slug
FROM repos r
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
WHERE r.deleted_at IS NULL
  AND r.visibility = 'public'
  AND (
    (r.owner_user_id IS NOT NULL AND owner_user.deleted_at IS NULL AND owner_user.suspended_at IS NULL)
    OR (r.owner_org_id IS NOT NULL AND owner_org.deleted_at IS NULL)
  )
  AND (
    r.owner_user_id = $1
    OR EXISTS (
      SELECT 1
      FROM org_members m
      WHERE m.org_id = r.owner_org_id
        AND m.user_id = $1
    )
    OR EXISTS (
      SELECT 1
      FROM repo_collaborators c
      WHERE c.repo_id = r.id
        AND c.user_id = $1
    )
  )
ORDER BY lower(COALESCE(owner_user.username::text, owner_org.slug::text, '')), lower(r.name::text), r.id;

-- name: ListPublicContributionRepos :many
SELECT sqlc.embed(r), COALESCE(u.username, o.slug)::text AS owner_slug
FROM repos r
LEFT JOIN users u ON u.id = r.owner_user_id
LEFT JOIN orgs o ON o.id = r.owner_org_id
WHERE r.deleted_at IS NULL
  AND r.visibility = 'public'
  AND (
    (r.owner_user_id IS NOT NULL AND u.deleted_at IS NULL AND u.suspended_at IS NULL)
    OR (r.owner_org_id IS NOT NULL AND o.deleted_at IS NULL)
  )
ORDER BY r.updated_at DESC, r.id DESC
LIMIT $1;

-- name: UpdateRepoGeneralSettings :exec
-- S32: General-tab settings persist via this single query so each
-- form post is one round-trip. The merge-method toggles are kept
-- separate from the repo create flow because they're admin-only.
--
-- PRO-EXT01-06pre: is_template added; the column is unconstrained at
-- the DB level so PRO-EXT01-06 can layer a handler-side gate that
-- restricts private templates to Pro users without a migration.
UPDATE repos
   SET description       = $2,
       has_issues        = $3,
       has_pulls         = $4,
       is_template       = $5,
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

-- ─── profile/org pinned repositories ───────────────────────────────

-- name: GetProfilePinSetForUser :one
SELECT id FROM profile_pin_sets WHERE owner_user_id = $1;

-- name: GetProfilePinSetForOrg :one
SELECT id FROM profile_pin_sets WHERE owner_org_id = $1;

-- name: UpsertProfilePinSetForUser :one
INSERT INTO profile_pin_sets (owner_user_id)
VALUES ($1)
ON CONFLICT (owner_user_id) WHERE owner_user_id IS NOT NULL
DO UPDATE SET updated_at = now()
RETURNING id;

-- name: UpsertProfilePinSetForOrg :one
INSERT INTO profile_pin_sets (owner_org_id)
VALUES ($1)
ON CONFLICT (owner_org_id) WHERE owner_org_id IS NOT NULL
DO UPDATE SET updated_at = now()
RETURNING id;

-- name: ListProfilePinsForSet :many
SELECT repo_id, position
FROM profile_pins
WHERE set_id = $1
ORDER BY position ASC;

-- name: DeleteProfilePinsForSet :exec
DELETE FROM profile_pins WHERE set_id = $1;

-- name: InsertProfilePin :exec
INSERT INTO profile_pins (set_id, repo_id, position)
VALUES ($1, $2, $3);

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

-- name: ListAllActiveReposWithOwner :many
-- Used by the GPG verification backfill (S51) to enumerate every
-- active repo system-wide. Unlike ListAllRepoFullNames this query
-- handles BOTH user-owned and org-owned repos via a COALESCE between
-- users.username and orgs.slug; the owner string is whatever
-- RepoFS.RepoPath expects.
SELECT
    r.id,
    r.name,
    r.default_branch,
    COALESCE(u.username, o.slug) AS owner
FROM repos r
LEFT JOIN users u ON u.id = r.owner_user_id
LEFT JOIN orgs  o ON o.id = r.owner_org_id
WHERE r.deleted_at IS NULL
ORDER BY r.id;

-- name: GetRepoForBackfill :one
-- Lookup the per-repo backfill metadata. Mirrors the row shape of
-- ListAllActiveReposWithOwner so the per-repo job handler can run
-- the same code path the bulk handler uses.
SELECT
    r.id,
    r.name,
    r.default_branch,
    COALESCE(u.username, o.slug) AS owner
FROM repos r
LEFT JOIN users u ON u.id = r.owner_user_id
LEFT JOIN orgs  o ON o.id = r.owner_org_id
WHERE r.id = $1 AND r.deleted_at IS NULL;

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
          last_indexed_oid, is_template;

-- name: CreateRepoFromTemplate :one
-- PRO-EXT01-06pre-b: insert a template-init shell. Mirrors
-- CreateForkRepo's `init_status='init_pending'` semantics so the
-- worker can flip to 'initialized' once the clone finishes, but
-- WITHOUT setting fork_of_repo_id — the new repo is independent
-- of the template (no alternates, no fork-count bump).
INSERT INTO repos (
    owner_user_id, owner_org_id, name, description, visibility,
    default_branch, init_status
) VALUES (
    $1, $2, $3, $4, $5, $6, 'init_pending'
)
RETURNING id, owner_user_id, owner_org_id, name, description, visibility,
          default_branch, is_archived, archived_at, deleted_at,
          disk_used_bytes, fork_of_repo_id, license_key, primary_language,
          has_issues, has_pulls, created_at, updated_at, default_branch_oid,
          allow_squash_merge, allow_rebase_merge, allow_merge_commit, default_merge_method,
          star_count, watcher_count, fork_count, init_status,
          last_indexed_oid, is_template;

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


-- name: AdminForceDeleteRepo :exec
-- Bypasses the soft-delete grace window (admin only — S34): set
-- deleted_at to a year ago so the next lifecycle sweep hard-deletes
-- without waiting. Replaces the inline UPDATE in admin/repos.go
-- (SR2 M2).
UPDATE repos SET deleted_at = now() - interval '1 year' WHERE id = $1;
