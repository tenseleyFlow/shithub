-- ─── activity feed / trending ─────────────────────────────────────

-- name: ListDashboardFeedEvents :many
SELECT
    de.id, de.actor_user_id, de.kind, de.repo_id, de.source_kind,
    de.source_id, de.public, de.payload, de.created_at,
    actor.username AS actor_username,
    actor.display_name AS actor_display_name,
    COALESCE(r.name::text, '')::text AS repo_name,
    COALESCE(r.description, '') AS repo_description,
    COALESCE(r.primary_language, '') AS repo_primary_language,
    COALESCE(r.star_count, 0)::bigint AS repo_star_count,
    COALESCE(r.fork_count, 0)::bigint AS repo_fork_count,
    COALESCE(owner_user.username::text, owner_org.slug::text, '')::text AS repo_owner,
    COALESCE(source_user.username::text, source_org.slug::text, '')::text AS source_name
FROM domain_events de
JOIN users actor ON actor.id = de.actor_user_id
LEFT JOIN repos r ON r.id = de.repo_id AND r.deleted_at IS NULL
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
LEFT JOIN users source_user ON de.source_kind = 'user' AND source_user.id = de.source_id
LEFT JOIN orgs source_org ON de.source_kind = 'org' AND source_org.id = de.source_id
WHERE de.public = true
  AND de.kind <> 'unstar'
  AND actor.suspended_at IS NULL
  AND actor.deleted_at IS NULL
  AND (
      de.repo_id IS NULL
      OR (r.id IS NOT NULL AND r.visibility = 'public')
  )
  AND (
      de.actor_user_id = sqlc.arg(viewer_user_id)::bigint
      OR de.actor_user_id IN (
          SELECT followee_user_id FROM follows
          WHERE follower_user_id = sqlc.arg(viewer_user_id)::bigint
            AND followee_user_id IS NOT NULL
      )
      OR de.repo_id IN (
          SELECT repo_id FROM watches
          WHERE user_id = sqlc.arg(viewer_user_id)::bigint
            AND level <> 'ignore'
      )
      OR (
          r.owner_org_id IN (
              SELECT followee_org_id FROM follows
              WHERE follower_user_id = sqlc.arg(viewer_user_id)::bigint
                AND followee_org_id IS NOT NULL
          )
      )
      OR (
          de.source_kind = 'org'
          AND de.source_id IN (
              SELECT followee_org_id FROM follows
              WHERE follower_user_id = sqlc.arg(viewer_user_id)::bigint
                AND followee_org_id IS NOT NULL
          )
      )
  )
  AND (
      sqlc.narg(before_created_at)::timestamptz IS NULL
      OR (de.created_at, de.id) < (
          sqlc.narg(before_created_at)::timestamptz,
          sqlc.narg(before_id)::bigint
      )
  )
ORDER BY de.created_at DESC, de.id DESC
LIMIT sqlc.arg(limit_count)::int;

-- name: ListPublicFeedEvents :many
SELECT
    de.id, de.actor_user_id, de.kind, de.repo_id, de.source_kind,
    de.source_id, de.public, de.payload, de.created_at,
    actor.username AS actor_username,
    actor.display_name AS actor_display_name,
    COALESCE(r.name::text, '')::text AS repo_name,
    COALESCE(r.description, '') AS repo_description,
    COALESCE(r.primary_language, '') AS repo_primary_language,
    COALESCE(r.star_count, 0)::bigint AS repo_star_count,
    COALESCE(r.fork_count, 0)::bigint AS repo_fork_count,
    COALESCE(owner_user.username::text, owner_org.slug::text, '')::text AS repo_owner,
    COALESCE(source_user.username::text, source_org.slug::text, '')::text AS source_name
FROM domain_events de
JOIN users actor ON actor.id = de.actor_user_id
LEFT JOIN repos r ON r.id = de.repo_id AND r.deleted_at IS NULL
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
LEFT JOIN users source_user ON de.source_kind = 'user' AND source_user.id = de.source_id
LEFT JOIN orgs source_org ON de.source_kind = 'org' AND source_org.id = de.source_id
WHERE de.public = true
  AND de.kind <> 'unstar'
  AND actor.suspended_at IS NULL
  AND actor.deleted_at IS NULL
  AND (
      de.repo_id IS NULL
      OR (r.id IS NOT NULL AND r.visibility = 'public')
  )
  AND (
      sqlc.narg(before_created_at)::timestamptz IS NULL
      OR (de.created_at, de.id) < (
          sqlc.narg(before_created_at)::timestamptz,
          sqlc.narg(before_id)::bigint
      )
  )
ORDER BY de.created_at DESC, de.id DESC
LIMIT sqlc.arg(limit_count)::int;

-- name: ListTrendingRepos :many
WITH recent AS (
    SELECT
        repo_id,
        (
            COUNT(*) FILTER (WHERE kind = 'star') * 3
          + COUNT(*) FILTER (WHERE kind = 'forked') * 2
          + COUNT(DISTINCT actor_user_id) FILTER (WHERE kind = 'push')
        )::bigint AS score
    FROM domain_events
    WHERE public = true
      AND repo_id IS NOT NULL
      AND created_at >= now() - make_interval(days => sqlc.arg(window_days)::int)
    GROUP BY repo_id
)
SELECT
    r.id AS repo_id,
    COALESCE(owner_user.username::text, owner_org.slug::text, '')::text AS owner,
    r.name::text AS name,
    r.description,
    COALESCE(r.primary_language, '') AS primary_language,
    r.star_count,
    r.fork_count,
    COALESCE(recent.score, 0)::bigint AS score,
    r.updated_at
FROM repos r
LEFT JOIN recent ON recent.repo_id = r.id
LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
WHERE r.visibility = 'public'
  AND r.deleted_at IS NULL
  AND r.is_archived = false
ORDER BY COALESCE(recent.score, 0) DESC, r.star_count DESC, r.updated_at DESC
LIMIT sqlc.arg(limit_count)::int;

-- name: ListTrendingUsers :many
WITH recent_events AS (
    SELECT actor_user_id AS user_id, COUNT(*)::bigint AS event_count
    FROM domain_events
    WHERE public = true
      AND actor_user_id IS NOT NULL
      AND created_at >= now() - make_interval(days => sqlc.arg(window_days)::int)
    GROUP BY actor_user_id
),
recent_followers AS (
    SELECT followee_user_id AS user_id, COUNT(*)::bigint AS follower_count
    FROM follows
    WHERE followee_user_id IS NOT NULL
      AND followed_at >= now() - make_interval(days => sqlc.arg(window_days)::int)
    GROUP BY followee_user_id
)
SELECT
    u.id AS user_id,
    u.username,
    u.display_name,
    (COALESCE(recent_followers.follower_count, 0) * 2 + COALESCE(recent_events.event_count, 0))::bigint AS score,
    COALESCE(recent_followers.follower_count, 0)::bigint AS follower_delta,
    COALESCE(recent_events.event_count, 0)::bigint AS event_count
FROM users u
LEFT JOIN recent_events ON recent_events.user_id = u.id
LEFT JOIN recent_followers ON recent_followers.user_id = u.id
WHERE u.suspended_at IS NULL
  AND u.deleted_at IS NULL
  AND (COALESCE(recent_followers.follower_count, 0) > 0 OR COALESCE(recent_events.event_count, 0) > 0)
ORDER BY score DESC, u.created_at DESC
LIMIT sqlc.arg(limit_count)::int;

-- name: ListDashboardReposForUser :many
WITH candidates AS (
    SELECT
        r.id AS repo_id,
        COALESCE(owner_user.username::text, owner_org.slug::text, '')::text AS owner,
        r.name::text AS name,
        r.description,
        r.visibility,
        COALESCE(r.primary_language, '') AS primary_language,
        r.star_count,
        r.fork_count,
        r.updated_at
    FROM repos r
    LEFT JOIN users owner_user ON owner_user.id = r.owner_user_id
    LEFT JOIN orgs owner_org ON owner_org.id = r.owner_org_id
    WHERE (
          r.owner_user_id = sqlc.arg(viewer_user_id)::bigint
          OR r.owner_org_id IN (
              SELECT org_id FROM org_members
              WHERE user_id = sqlc.arg(viewer_user_id)::bigint
          )
      )
      AND r.deleted_at IS NULL
)
SELECT
    r.repo_id,
    r.owner,
    r.name::text AS name,
    r.description,
    r.visibility,
    r.primary_language,
    r.star_count,
    r.fork_count,
    r.updated_at
FROM candidates r
WHERE (
    sqlc.arg(search_query)::text = ''
    OR lower(r.owner || '/' || r.name::text) LIKE '%' || lower(sqlc.arg(search_query)::text) || '%'
    OR lower(r.owner) LIKE '%' || lower(sqlc.arg(search_query)::text) || '%'
    OR lower(r.name::text) LIKE '%' || lower(sqlc.arg(search_query)::text) || '%'
    OR lower(r.description) LIKE '%' || lower(sqlc.arg(search_query)::text) || '%'
)
ORDER BY
    CASE
        WHEN sqlc.arg(search_query)::text = '' THEN 0
        WHEN lower(r.owner || '/' || r.name::text) = lower(sqlc.arg(search_query)::text) THEN 0
        WHEN lower(r.name::text) = lower(sqlc.arg(search_query)::text) THEN 1
        WHEN lower(r.owner || '/' || r.name::text) LIKE lower(sqlc.arg(search_query)::text) || '%' THEN 2
        WHEN lower(r.name::text) LIKE lower(sqlc.arg(search_query)::text) || '%' THEN 3
        WHEN lower(r.owner) LIKE lower(sqlc.arg(search_query)::text) || '%' THEN 4
        ELSE 5
    END,
    r.updated_at DESC,
    lower(r.owner),
    lower(r.name::text)
LIMIT sqlc.arg(limit_count)::int;

-- name: InsertTrendingSnapshot :one
INSERT INTO trending_snapshots (scope, kind, payload)
VALUES ($1, $2, $3)
RETURNING id, scope, kind, captured_at, payload;

-- name: LatestTrendingSnapshot :one
SELECT id, scope, kind, captured_at, payload
FROM trending_snapshots
WHERE scope = $1
  AND kind = $2
ORDER BY captured_at DESC
LIMIT 1;
