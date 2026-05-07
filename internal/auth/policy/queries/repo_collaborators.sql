-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Query surface for the policy package. The policy code reads roles to
-- decide; admin/settings UIs (S32) write through these as well.

-- name: GetCollabRole :one
-- Returns the collaborator role for (repo_id, user_id), or pgx.ErrNoRows
-- when the user is not a collaborator on this repo.
SELECT role FROM repo_collaborators
WHERE repo_id = $1 AND user_id = $2;

-- name: UpsertCollabRole :exec
-- Insert or upgrade a collaborator's role. added_by_user_id records who
-- granted the role for the audit trail.
INSERT INTO repo_collaborators (repo_id, user_id, role, added_by_user_id)
VALUES ($1, $2, $3, sqlc.narg(added_by_user_id)::bigint)
ON CONFLICT (repo_id, user_id)
DO UPDATE SET role = EXCLUDED.role,
              added_by_user_id = EXCLUDED.added_by_user_id;

-- name: RemoveCollab :exec
DELETE FROM repo_collaborators WHERE repo_id = $1 AND user_id = $2;

-- name: ListCollabs :many
-- Used by the repo settings page (S32) and tests.
SELECT rc.repo_id, rc.user_id, rc.role, rc.added_at,
       u.username AS username, u.display_name AS display_name
FROM repo_collaborators rc
JOIN users u ON u.id = rc.user_id
WHERE rc.repo_id = $1
ORDER BY rc.added_at DESC;
