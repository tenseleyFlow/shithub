-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── teams ─────────────────────────────────────────────────────────

-- name: CreateTeam :one
INSERT INTO teams (org_id, slug, display_name, description, parent_team_id, privacy, created_by_user_id)
VALUES ($1, $2, $3, $4, sqlc.narg(parent_team_id)::bigint, $5, sqlc.narg(created_by_user_id)::bigint)
RETURNING *;

-- name: GetTeamByID :one
SELECT * FROM teams WHERE id = $1;

-- name: GetTeamByOrgAndSlug :one
SELECT * FROM teams WHERE org_id = $1 AND slug = $2;

-- name: ListTeamsForOrg :many
SELECT * FROM teams WHERE org_id = $1 ORDER BY slug ASC;

-- name: ListChildTeams :many
SELECT * FROM teams WHERE parent_team_id = $1 ORDER BY slug ASC;

-- name: UpdateTeamProfile :exec
UPDATE teams SET display_name = $2, description = $3, privacy = $4, updated_at = now()
WHERE id = $1;

-- name: SetTeamParent :exec
-- The one-level-nesting BEFORE trigger blocks invalid moves; the
-- caller surfaces the SQLSTATE 23514 as a friendly error.
UPDATE teams SET parent_team_id = sqlc.narg(parent_team_id)::bigint, updated_at = now()
WHERE id = $1;

-- name: DeleteTeam :exec
-- Children's parent_team_id flips to NULL via the FK ON DELETE SET NULL,
-- promoting them to top-level — matches the "deleting parent doesn't
-- delete children" intent in the spec's pitfalls.
DELETE FROM teams WHERE id = $1;

-- ─── team_members ─────────────────────────────────────────────────

-- name: AddTeamMember :exec
INSERT INTO team_members (team_id, user_id, role, added_by_user_id)
VALUES ($1, $2, $3, sqlc.narg(added_by_user_id)::bigint)
ON CONFLICT (team_id, user_id) DO NOTHING;

-- name: SetTeamMemberRole :exec
UPDATE team_members SET role = $3 WHERE team_id = $1 AND user_id = $2;

-- name: RemoveTeamMember :exec
DELETE FROM team_members WHERE team_id = $1 AND user_id = $2;

-- name: ListTeamMembers :many
SELECT m.team_id, m.user_id, m.role, m.added_by_user_id, m.added_at,
       u.username, u.display_name
FROM team_members m
JOIN users u ON u.id = m.user_id
WHERE m.team_id = $1 AND u.deleted_at IS NULL
ORDER BY m.role ASC, u.username ASC;

-- name: ListTeamsForUserInOrg :many
-- Returns the teams a user directly belongs to within an org. The
-- policy aggregator unions this with each row's parent_team_id to
-- get the inherited set.
SELECT t.id, t.org_id, t.slug, t.display_name, t.description,
       t.parent_team_id, t.privacy, t.created_by_user_id,
       t.created_at, t.updated_at,
       m.role AS member_role
FROM team_members m
JOIN teams t ON t.id = m.team_id
WHERE t.org_id = $1 AND m.user_id = $2;

-- ─── team_repo_access ─────────────────────────────────────────────

-- name: GrantTeamRepoAccess :exec
INSERT INTO team_repo_access (team_id, repo_id, role, added_by_user_id)
VALUES ($1, $2, $3, sqlc.narg(added_by_user_id)::bigint)
ON CONFLICT (team_id, repo_id) DO UPDATE SET role = EXCLUDED.role;

-- name: RevokeTeamRepoAccess :exec
DELETE FROM team_repo_access WHERE team_id = $1 AND repo_id = $2;

-- name: GetTeamRepoAccess :one
SELECT * FROM team_repo_access WHERE team_id = $1 AND repo_id = $2;

-- name: ListTeamRepoAccess :many
-- All repos the team has any grant on; for the team-view page.
SELECT a.team_id, a.repo_id, a.role, a.added_by_user_id, a.added_at,
       r.name AS repo_name, r.visibility::text AS visibility
FROM team_repo_access a
JOIN repos r ON r.id = a.repo_id
WHERE a.team_id = $1 AND r.deleted_at IS NULL
ORDER BY r.name ASC;

-- name: ListRepoTeamGrants :many
-- All teams with grants on a single repo; for repo settings/access page.
SELECT a.team_id, a.repo_id, a.role, a.added_by_user_id, a.added_at,
       t.slug AS team_slug, t.display_name AS team_display_name,
       t.privacy::text AS team_privacy
FROM team_repo_access a
JOIN teams t ON t.id = a.team_id
WHERE a.repo_id = $1
ORDER BY t.slug ASC;

-- name: ListTeamAccessForRepoAndTeams :many
-- Policy hot-path: given a repo and a set of team_ids the actor
-- belongs to (incl. inherited), return the access rows. Caller picks
-- the highest role.
SELECT team_id, repo_id, role
FROM team_repo_access
WHERE repo_id = $1 AND team_id = ANY($2::bigint[]);
