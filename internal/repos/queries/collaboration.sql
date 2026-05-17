-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── repo projects ─────────────────────────────────────────────────

-- name: CreateRepoProject :one
INSERT INTO repo_projects (repo_id, title, description, created_by_user_id)
VALUES ($1, $2, $3, sqlc.narg(created_by_user_id)::bigint)
RETURNING *;

-- name: ListRepoProjects :many
SELECT *
FROM repo_projects
WHERE repo_id = $1
ORDER BY state ASC, updated_at DESC, id DESC;

-- name: GetRepoProject :one
SELECT *
FROM repo_projects
WHERE id = $1 AND repo_id = $2;

-- name: UpdateRepoProject :one
UPDATE repo_projects
SET title = $3,
    description = $4
WHERE id = $1 AND repo_id = $2
RETURNING *;

-- name: SetRepoProjectState :one
UPDATE repo_projects
SET state = $3,
    closed_at = CASE WHEN $3::repo_project_state = 'closed' THEN now() ELSE NULL END
WHERE id = $1 AND repo_id = $2
RETURNING *;

-- name: DeleteRepoProject :exec
DELETE FROM repo_projects
WHERE id = $1 AND repo_id = $2;

-- name: AddIssueToRepoProject :one
INSERT INTO repo_project_items (project_id, issue_id, added_by_user_id)
VALUES ($1, $2, sqlc.narg(added_by_user_id)::bigint)
ON CONFLICT (project_id, issue_id) DO UPDATE
SET added_by_user_id = excluded.added_by_user_id
RETURNING *;

-- name: RemoveIssueFromRepoProject :exec
DELETE FROM repo_project_items
WHERE project_id = $1 AND issue_id = $2;

-- name: ListRepoProjectItems :many
SELECT pi.id,
       pi.project_id,
       pi.issue_id,
       pi.added_by_user_id,
       pi.created_at,
       i.number AS issue_number,
       i.kind AS issue_kind,
       i.title AS issue_title,
       i.state AS issue_state
FROM repo_project_items pi
JOIN issues i ON i.id = pi.issue_id
WHERE pi.project_id = $1
ORDER BY pi.created_at DESC, pi.id DESC;

-- name: ListRepoProjectsForIssue :many
SELECT p.id,
       p.repo_id,
       p.title,
       p.description,
       p.state,
       p.created_by_user_id,
       p.closed_at,
       p.created_at,
       p.updated_at
FROM repo_project_items pi
JOIN repo_projects p ON p.id = pi.project_id
WHERE pi.issue_id = $1
ORDER BY p.state ASC, p.updated_at DESC, p.id DESC;

-- ─── repo wiki pages ───────────────────────────────────────────────

-- name: CreateRepoWikiPage :one
INSERT INTO repo_wiki_pages (
    repo_id, slug, title, body, body_html_cached, created_by_user_id, updated_by_user_id
) VALUES (
    $1, $2, $3, $4, sqlc.narg(body_html_cached)::text,
    sqlc.narg(created_by_user_id)::bigint,
    sqlc.narg(updated_by_user_id)::bigint
)
RETURNING *;

-- name: ListRepoWikiPages :many
SELECT *
FROM repo_wiki_pages
WHERE repo_id = $1
ORDER BY CASE WHEN slug = 'home' THEN 0 ELSE 1 END, title ASC, id ASC;

-- name: CountRepoWikiPages :one
SELECT count(*) FROM repo_wiki_pages WHERE repo_id = $1;

-- name: GetRepoWikiPageBySlug :one
SELECT *
FROM repo_wiki_pages
WHERE repo_id = $1 AND slug = $2;

-- name: UpdateRepoWikiPage :one
UPDATE repo_wiki_pages
SET title = $3,
    body = $4,
    body_html_cached = sqlc.narg(body_html_cached)::text,
    updated_by_user_id = sqlc.narg(updated_by_user_id)::bigint
WHERE id = $1 AND repo_id = $2
RETURNING *;

-- name: DeleteRepoWikiPage :exec
DELETE FROM repo_wiki_pages
WHERE id = $1 AND repo_id = $2;
