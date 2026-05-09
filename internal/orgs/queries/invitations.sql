-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── org_invitations ───────────────────────────────────────────────

-- name: CreateOrgInvitation :one
INSERT INTO org_invitations (
    org_id, invited_by_user_id, target_user_id, target_email,
    role, token_hash, expires_at
) VALUES (
    $1, $2, sqlc.narg(target_user_id)::bigint, sqlc.narg(target_email)::citext,
    $3, $4, $5
)
RETURNING *;

-- name: GetOrgInvitationByTokenHash :one
SELECT * FROM org_invitations WHERE token_hash = $1;

-- name: GetOrgInvitationByID :one
SELECT * FROM org_invitations WHERE id = $1;

-- name: ListPendingInvitationsForOrg :many
SELECT i.*, u.username AS target_username, ib.username AS invited_by_username
FROM org_invitations i
LEFT JOIN users u  ON u.id  = i.target_user_id
LEFT JOIN users ib ON ib.id = i.invited_by_user_id
WHERE i.org_id = $1
  AND i.accepted_at IS NULL
  AND i.declined_at IS NULL
  AND i.canceled_at IS NULL
  AND i.expires_at > now()
ORDER BY i.created_at DESC;

-- name: ListPendingInvitationsForUser :many
-- Two flavors: by user_id (already-claimed invites) and by email
-- (claim-on-signup). Caller unions the two lookups when surfacing
-- to the user.
SELECT i.*, o.slug AS org_slug, o.display_name AS org_display_name
FROM org_invitations i
JOIN orgs o ON o.id = i.org_id
WHERE i.target_user_id = $1
  AND i.accepted_at IS NULL
  AND i.declined_at IS NULL
  AND i.canceled_at IS NULL
  AND i.expires_at > now()
  AND o.deleted_at IS NULL
ORDER BY i.created_at DESC;

-- name: ListPendingInvitationsForEmail :many
SELECT i.*, o.slug AS org_slug, o.display_name AS org_display_name
FROM org_invitations i
JOIN orgs o ON o.id = i.org_id
WHERE i.target_email = $1
  AND i.accepted_at IS NULL
  AND i.declined_at IS NULL
  AND i.canceled_at IS NULL
  AND i.expires_at > now()
  AND o.deleted_at IS NULL
ORDER BY i.created_at DESC;

-- name: AcceptOrgInvitation :exec
UPDATE org_invitations SET accepted_at = now() WHERE id = $1;

-- name: DeclineOrgInvitation :exec
UPDATE org_invitations SET declined_at = now() WHERE id = $1;

-- name: CancelOrgInvitation :exec
UPDATE org_invitations SET canceled_at = now() WHERE id = $1;

-- name: GetExistingPendingInvitation :one
-- Idempotency check before creating a new invite — so a re-issued
-- invite to the same target doesn't accumulate stale rows.
SELECT * FROM org_invitations
WHERE org_id = $1
  AND ( (target_user_id = sqlc.narg(target_user_id)::bigint)
     OR (target_email   = sqlc.narg(target_email)::citext) )
  AND accepted_at IS NULL
  AND declined_at IS NULL
  AND canceled_at IS NULL
  AND expires_at > now()
LIMIT 1;
