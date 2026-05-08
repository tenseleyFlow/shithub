-- ─── stars ─────────────────────────────────────────────────────────

-- name: InsertStar :exec
-- ON CONFLICT DO NOTHING is the idempotency guard: re-starring an
-- already-starred repo doesn't double-increment the count (the
-- AFTER INSERT trigger only fires on actual insert).
INSERT INTO stars (user_id, repo_id) VALUES ($1, $2)
ON CONFLICT (user_id, repo_id) DO NOTHING;

-- name: DeleteStar :exec
DELETE FROM stars WHERE user_id = $1 AND repo_id = $2;

-- name: HasStar :one
SELECT EXISTS (
    SELECT 1 FROM stars WHERE user_id = $1 AND repo_id = $2
) AS has_star;

-- name: ListStargazersForRepo :many
-- Public-repo stargazer list. Paginated by `starred_at DESC`.
-- Excludes suspended users so they don't taint public lists. The
-- private-repo gate is at the handler layer (policy.IsVisibleTo).
SELECT s.user_id, s.starred_at, u.username, u.display_name
FROM stars s
JOIN users u ON u.id = s.user_id
WHERE s.repo_id = $1
  AND u.suspended_at IS NULL
ORDER BY s.starred_at DESC
LIMIT $2 OFFSET $3;

-- name: CountStargazersForRepo :one
SELECT COUNT(*) FROM stars s
JOIN users u ON u.id = s.user_id
WHERE s.repo_id = $1
  AND u.suspended_at IS NULL;

-- name: ListStarsForUser :many
-- The "Stars" profile tab. The handler layer post-filters for repo
-- visibility against the viewer; this query returns everything the
-- user starred and lets the handler decide what to render. Sort axis
-- is the spec's day-1 lean: most-recently-starred first.
SELECT s.repo_id, s.starred_at,
       r.name AS repo_name, r.description, r.visibility,
       r.star_count, r.primary_language, r.updated_at,
       r.owner_user_id, r.owner_org_id
FROM stars s
JOIN repos r ON r.id = s.repo_id
WHERE s.user_id = $1
  AND r.deleted_at IS NULL
ORDER BY s.starred_at DESC
LIMIT $2 OFFSET $3;

-- name: CountStarsForUser :one
SELECT COUNT(*) FROM stars s
JOIN repos r ON r.id = s.repo_id
WHERE s.user_id = $1
  AND r.deleted_at IS NULL;
