-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: BumpAuthThrottle :one
-- Increments the hit counter for (scope, identifier). When the existing
-- window is older than the supplied window-start cutoff, resets to 1 and
-- starts a new window. Returns the post-bump (hits, window_started_at).
INSERT INTO auth_throttle (scope, identifier, hits, window_started_at)
VALUES ($1, $2, 1, now())
ON CONFLICT (scope, identifier) DO UPDATE
SET hits = CASE
        WHEN auth_throttle.window_started_at < $3 THEN 1
        ELSE auth_throttle.hits + 1
    END,
    window_started_at = CASE
        WHEN auth_throttle.window_started_at < $3 THEN now()
        ELSE auth_throttle.window_started_at
    END
RETURNING hits, window_started_at;

-- name: ResetAuthThrottle :exec
DELETE FROM auth_throttle WHERE scope = $1 AND identifier = $2;

-- name: PurgeStaleAuthThrottle :exec
DELETE FROM auth_throttle WHERE window_started_at < $1;
