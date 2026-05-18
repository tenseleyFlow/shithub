-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertRepoTrafficUnique :one
INSERT INTO repo_traffic_uniques (
    repo_id, day, metric, key, visitor_hash
) VALUES (
    $1, $2, $3, $4, $5
)
ON CONFLICT DO NOTHING
RETURNING true::boolean AS inserted;

-- name: UpsertRepoTrafficDailyView :exec
INSERT INTO repo_traffic_daily (
    repo_id, day, views, unique_views
) VALUES (
    $1, $2, 1, $3
)
ON CONFLICT (repo_id, day) DO UPDATE SET
    views = repo_traffic_daily.views + 1,
    unique_views = repo_traffic_daily.unique_views + EXCLUDED.unique_views,
    updated_at = now();

-- name: UpsertRepoTrafficDailyClone :exec
INSERT INTO repo_traffic_daily (
    repo_id, day, clones, unique_clones
) VALUES (
    $1, $2, 1, $3
)
ON CONFLICT (repo_id, day) DO UPDATE SET
    clones = repo_traffic_daily.clones + 1,
    unique_clones = repo_traffic_daily.unique_clones + EXCLUDED.unique_clones,
    updated_at = now();

-- name: UpsertRepoTrafficPathView :exec
INSERT INTO repo_traffic_paths (
    repo_id, day, path, views, unique_views
) VALUES (
    $1, $2, $3, 1, $4
)
ON CONFLICT (repo_id, day, path) DO UPDATE SET
    views = repo_traffic_paths.views + 1,
    unique_views = repo_traffic_paths.unique_views + EXCLUDED.unique_views,
    updated_at = now();

-- name: UpsertRepoTrafficReferrerView :exec
INSERT INTO repo_traffic_referrers (
    repo_id, day, referrer, views, unique_views
) VALUES (
    $1, $2, $3, 1, $4
)
ON CONFLICT (repo_id, day, referrer) DO UPDATE SET
    views = repo_traffic_referrers.views + 1,
    unique_views = repo_traffic_referrers.unique_views + EXCLUDED.unique_views,
    updated_at = now();

-- name: ListRepoTrafficDaily :many
SELECT repo_id, day, views, unique_views, clones, unique_clones,
       created_at, updated_at
FROM repo_traffic_daily
WHERE repo_id = $1
  AND day >= $2
ORDER BY day ASC;

-- name: ListRepoTrafficPaths :many
SELECT path,
       sum(views)::bigint AS views,
       sum(unique_views)::bigint AS unique_views
FROM repo_traffic_paths
WHERE repo_id = $1
  AND day >= $2
GROUP BY path
ORDER BY views DESC, path ASC
LIMIT $3;

-- name: ListRepoTrafficReferrers :many
SELECT referrer,
       sum(views)::bigint AS views,
       sum(unique_views)::bigint AS unique_views
FROM repo_traffic_referrers
WHERE repo_id = $1
  AND day >= $2
GROUP BY referrer
ORDER BY views DESC, referrer ASC
LIMIT $3;
