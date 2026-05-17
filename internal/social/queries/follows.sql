-- ─── follows ───────────────────────────────────────────────────────

-- name: FollowUser :one
WITH inserted AS (
    INSERT INTO follows (follower_user_id, followee_user_id)
    VALUES ($1, $2)
    ON CONFLICT (follower_user_id, followee_user_id)
        WHERE followee_user_id IS NOT NULL
    DO NOTHING
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM inserted) AS inserted;

-- name: UnfollowUser :execrows
DELETE FROM follows
WHERE follower_user_id = $1
  AND followee_user_id = $2;

-- name: FollowOrg :one
WITH inserted AS (
    INSERT INTO follows (follower_user_id, followee_org_id)
    VALUES ($1, $2)
    ON CONFLICT (follower_user_id, followee_org_id)
        WHERE followee_org_id IS NOT NULL
    DO NOTHING
    RETURNING 1
)
SELECT EXISTS (SELECT 1 FROM inserted) AS inserted;

-- name: UnfollowOrg :execrows
DELETE FROM follows
WHERE follower_user_id = $1
  AND followee_org_id = $2;

-- name: IsFollowingUser :one
SELECT EXISTS (
    SELECT 1 FROM follows
    WHERE follower_user_id = $1
      AND followee_user_id = $2
) AS following;

-- name: IsFollowingOrg :one
SELECT EXISTS (
    SELECT 1 FROM follows
    WHERE follower_user_id = $1
      AND followee_org_id = $2
) AS following;

-- name: CountFollowersForUser :one
SELECT COUNT(*) FROM follows f
JOIN users u ON u.id = f.follower_user_id
WHERE f.followee_user_id = $1
  AND u.suspended_at IS NULL
  AND u.deleted_at IS NULL;

-- name: CountFollowingForUser :one
SELECT COUNT(*) FROM follows f
LEFT JOIN users u ON u.id = f.followee_user_id
LEFT JOIN orgs o ON o.id = f.followee_org_id
WHERE f.follower_user_id = $1
  AND (f.followee_user_id IS NULL OR (u.suspended_at IS NULL AND u.deleted_at IS NULL))
  AND (f.followee_org_id IS NULL OR (o.suspended_at IS NULL AND o.deleted_at IS NULL));

-- name: CountFollowersForOrg :one
SELECT COUNT(*) FROM follows f
JOIN users u ON u.id = f.follower_user_id
WHERE f.followee_org_id = $1
  AND u.suspended_at IS NULL
  AND u.deleted_at IS NULL;

-- name: ListFollowersForUser :many
-- PRO-EXT_SR2-15: select u.plan so the follows list renders a Pro
-- pill next to Pro users (matches the discovery-surface treatment
-- applied to every other user-bearing template).
SELECT f.follower_user_id AS user_id, f.followed_at, u.username, u.display_name, u.plan
FROM follows f
JOIN users u ON u.id = f.follower_user_id
WHERE f.followee_user_id = $1
  AND u.suspended_at IS NULL
  AND u.deleted_at IS NULL
ORDER BY f.followed_at DESC, f.id DESC
LIMIT $2 OFFSET $3;

-- name: ListFollowingUsersForUser :many
-- PRO-EXT_SR2-15: same Pro-pill rationale as ListFollowersForUser.
SELECT f.followee_user_id AS user_id, f.followed_at, u.username, u.display_name, u.plan
FROM follows f
JOIN users u ON u.id = f.followee_user_id
WHERE f.follower_user_id = $1
  AND u.suspended_at IS NULL
  AND u.deleted_at IS NULL
ORDER BY f.followed_at DESC, f.id DESC
LIMIT $2 OFFSET $3;

-- name: ListFollowingOrgsForUser :many
SELECT f.followee_org_id AS org_id, f.followed_at, o.slug, o.display_name
FROM follows f
JOIN orgs o ON o.id = f.followee_org_id
WHERE f.follower_user_id = $1
  AND o.suspended_at IS NULL
  AND o.deleted_at IS NULL
ORDER BY f.followed_at DESC, f.id DESC
LIMIT $2 OFFSET $3;

-- name: ListFollowersForOrg :many
-- PRO-EXT_SR2-15: same Pro-pill rationale as ListFollowersForUser.
SELECT f.follower_user_id AS user_id, f.followed_at, u.username, u.display_name, u.plan
FROM follows f
JOIN users u ON u.id = f.follower_user_id
WHERE f.followee_org_id = $1
  AND u.suspended_at IS NULL
  AND u.deleted_at IS NULL
ORDER BY f.followed_at DESC, f.id DESC
LIMIT $2 OFFSET $3;
