-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertUserTokenUsageEvent :exec
-- Fire-and-forget from the PAT middleware. Non-fatal: a failed insert
-- never affects request authorization.
INSERT INTO user_token_usage_events (token_id, method, route_prefix, status_code)
VALUES ($1, $2, $3, $4);

-- name: CountUserTokenUsageSince :one
-- Total request count for the token since `since`. Used for the
-- analytics summary card.
SELECT count(*) FROM user_token_usage_events
WHERE token_id = $1 AND occurred_at >= $2;

-- name: ListUserTokenUsageByDay :many
-- Day-bucketed counts for the analytics chart. Returns at most one
-- row per day; gaps mean zero traffic that day (the renderer fills
-- them in).
SELECT date_trunc('day', occurred_at)::timestamptz AS day, count(*)::bigint AS event_count
FROM user_token_usage_events
WHERE token_id = $1 AND occurred_at >= $2
GROUP BY day
ORDER BY day;

-- name: ListUserTokenTopRoutes :many
-- Top N (method, route_prefix) tuples by request count over a window.
-- Drives the "where this token is being used" table on the analytics
-- page.
SELECT method, route_prefix, count(*)::bigint AS event_count
FROM user_token_usage_events
WHERE token_id = $1 AND occurred_at >= $2
GROUP BY method, route_prefix
ORDER BY event_count DESC
LIMIT $3;

-- name: GetUserTokenByIDForUser :one
-- Scoped fetch: only returns the row if it belongs to user_id. Used by
-- the analytics handler to verify ownership before rendering.
SELECT id, user_id, name, token_hash, token_prefix, scopes,
       expires_at, last_used_at, last_used_ip, revoked_at, created_at,
       ip_allowlist, repo_id
FROM user_tokens
WHERE id = $1 AND user_id = $2;
