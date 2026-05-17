-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── org_members ───────────────────────────────────────────────────

-- name: AddOrgMember :exec
-- Idempotent on (org_id, user_id): re-adding a member is a no-op
-- rather than an error. The role is taken from the supplied row when
-- the member is new; existing rows keep their current role (use
-- ChangeOrgMemberRole to update).
INSERT INTO org_members (org_id, user_id, role, invited_by_user_id)
VALUES ($1, $2, $3, sqlc.narg(invited_by_user_id)::bigint)
ON CONFLICT (org_id, user_id) DO NOTHING;

-- name: RemoveOrgMember :exec
DELETE FROM org_members WHERE org_id = $1 AND user_id = $2;

-- name: ChangeOrgMemberRole :exec
UPDATE org_members SET role = $3 WHERE org_id = $1 AND user_id = $2;

-- name: GetOrgMember :one
SELECT * FROM org_members WHERE org_id = $1 AND user_id = $2;

-- name: ListOrgMembers :many
-- Members of an org with usernames + roles for the people page.
-- PRO-EXT_SR2-15: select u.plan so the people list renders a Pro
-- badge next to Pro user accounts (org owners get the org-paid
-- treatment in a separate sprint).
SELECT m.org_id, m.user_id, m.role, m.invited_by_user_id, m.joined_at,
       u.username, u.display_name, u.plan
FROM org_members m
JOIN users u ON u.id = m.user_id
WHERE m.org_id = $1
  AND u.deleted_at IS NULL
ORDER BY m.role ASC, u.username ASC;

-- name: CountOrgOwners :one
-- Used by the last-owner protection: refuses to remove or demote the
-- only owner. Caller compares `count = 1` before allowing the change.
SELECT count(*) FROM org_members WHERE org_id = $1 AND role = 'owner';

-- name: ListOrgsForUser :many
-- Profile-page input: every org a user is a member of, with role.
SELECT m.org_id, m.role, o.slug, o.display_name, o.avatar_object_key
FROM org_members m
JOIN orgs o ON o.id = m.org_id
WHERE m.user_id = $1
  AND o.deleted_at IS NULL
ORDER BY o.slug ASC;
