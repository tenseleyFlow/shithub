-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Generic rate-limit counter queries (S35). Two write paths:
--   * BumpRateLimit — atomic UPSERT that rolls the window forward
--                     when stale, increments hits otherwise. Returns
--                     the post-update hits + window_started_at so
--                     the caller can compute Retry-After without a
--                     second round trip.
--   * BumpSignupIPThrottle — same shape against signup_ip_throttle
--                            keyed by inet/CIDR.
--
-- Reads (PeekRateLimit, PeekSignupIPThrottle) are kept around for
-- the admin observability surface; the hot path uses Bump-and-decide.

-- name: BumpRateLimit :one
-- Roll-or-increment in one statement. The CASE in the UPDATE branch
-- handles the window roll: when the existing window started before
-- (now - $3 interval), we treat it as a new window and reset hits
-- to 1; otherwise we increment in place.
INSERT INTO rate_limits (scope, key, hits, window_started_at)
VALUES (sqlc.arg(scope), sqlc.arg(key), 1, now())
ON CONFLICT (scope, key)
DO UPDATE SET
    hits              = CASE
                          WHEN rate_limits.window_started_at < now() - sqlc.arg(ttl)::interval
                          THEN 1
                          ELSE rate_limits.hits + 1
                        END,
    window_started_at = CASE
                          WHEN rate_limits.window_started_at < now() - sqlc.arg(ttl)::interval
                          THEN now()
                          ELSE rate_limits.window_started_at
                        END
RETURNING hits, window_started_at;

-- name: PeekRateLimit :one
SELECT scope, key, hits, window_started_at
FROM rate_limits
WHERE scope = $1 AND key = $2;

-- name: AcquireRateLimitLease :one
-- Concurrent-lease variant for long-lived streams. `hits` is the
-- currently-held lease count. The ttl rolls stale rows forward so a process
-- crash or severed TCP connection cannot consume capacity indefinitely.
INSERT INTO rate_limits (scope, key, hits, window_started_at)
VALUES (sqlc.arg(scope), sqlc.arg(key), 1, now())
ON CONFLICT (scope, key)
DO UPDATE SET
    hits              = CASE
                          WHEN rate_limits.window_started_at < now() - sqlc.arg(ttl)::interval
                          THEN 1
                          ELSE rate_limits.hits + 1
                        END,
    window_started_at = CASE
                          WHEN rate_limits.window_started_at < now() - sqlc.arg(ttl)::interval
                          THEN now()
                          ELSE rate_limits.window_started_at
                        END
WHERE rate_limits.window_started_at < now() - sqlc.arg(ttl)::interval
   OR rate_limits.hits < sqlc.arg(max_hits)::integer
RETURNING hits, window_started_at;

-- name: ReleaseRateLimitLease :execrows
UPDATE rate_limits
SET hits = GREATEST(hits - 1, 0)
WHERE scope = $1 AND key = $2;

-- name: PruneRateLimits :execrows
DELETE FROM rate_limits
WHERE window_started_at < now() - sqlc.arg(retention)::interval;

-- name: BumpSignupIPThrottle :one
-- Same UPSERT shape against the inet-keyed signup throttle.
INSERT INTO signup_ip_throttle (cidr, hits, window_started_at)
VALUES (sqlc.arg(cidr), 1, now())
ON CONFLICT (cidr)
DO UPDATE SET
    hits              = CASE
                          WHEN signup_ip_throttle.window_started_at < now() - sqlc.arg(ttl)::interval
                          THEN 1
                          ELSE signup_ip_throttle.hits + 1
                        END,
    window_started_at = CASE
                          WHEN signup_ip_throttle.window_started_at < now() - sqlc.arg(ttl)::interval
                          THEN now()
                          ELSE signup_ip_throttle.window_started_at
                        END
RETURNING hits, window_started_at;

-- name: PruneSignupIPThrottle :execrows
DELETE FROM signup_ip_throttle
WHERE window_started_at < now() - sqlc.arg(retention)::interval;
