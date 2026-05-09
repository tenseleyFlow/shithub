-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── orgs ──────────────────────────────────────────────────────────

-- name: CreateOrg :one
-- Inserts a new org. The trigger on `orgs` populates `principals` for
-- /{slug} resolution; the caller adds the creator as owner in the same
-- tx (separate query so the orchestrator owns ordering).
INSERT INTO orgs (slug, display_name, description, billing_email, created_by_user_id)
VALUES ($1, $2, $3, $4, sqlc.narg(created_by_user_id)::bigint)
RETURNING *;

-- name: GetOrgByID :one
SELECT * FROM orgs WHERE id = $1;

-- name: GetOrgBySlug :one
-- Slug is citext so the comparison is case-insensitive. Soft-deleted
-- orgs are filtered out so a slug freed by deletion is invisible to
-- normal lookups (the resolver still sees the row via the deleted_at
-- column for grace-period restore).
SELECT * FROM orgs
WHERE slug = $1
  AND deleted_at IS NULL;

-- name: GetOrgBySlugIncludingDeleted :one
SELECT * FROM orgs WHERE slug = $1;

-- name: UpdateOrgProfile :exec
UPDATE orgs
   SET display_name = $2,
       description  = $3,
       location     = $4,
       website      = $5,
       billing_email = $6,
       updated_at   = now()
 WHERE id = $1;

-- name: SetOrgAvatarKey :exec
UPDATE orgs SET avatar_object_key = $2, updated_at = now() WHERE id = $1;

-- name: SetOrgAllowMemberRepoCreate :exec
UPDATE orgs SET allow_member_repo_create = $2, updated_at = now() WHERE id = $1;

-- name: SetOrgSuspended :exec
UPDATE orgs
   SET suspended_at = CASE WHEN $2::boolean THEN now() ELSE NULL END,
       suspended_reason = CASE WHEN $2::boolean THEN $3 ELSE NULL END,
       updated_at = now()
 WHERE id = $1;

-- name: SoftDeleteOrg :exec
UPDATE orgs SET deleted_at = now(), updated_at = now() WHERE id = $1;

-- name: RestoreOrg :exec
UPDATE orgs SET deleted_at = NULL, updated_at = now() WHERE id = $1;

-- ─── principals (read-only from this domain) ───────────────────────

-- name: ResolvePrincipal :one
-- Single-query /{slug} resolver. Returns the (kind, id) tuple that
-- /{slug}/* routes use to dispatch to the user-profile or org-profile
-- handler. Both kinds share the slug PK so collisions are
-- impossible at the DB layer.
SELECT slug, kind, id FROM principals WHERE slug = $1;
