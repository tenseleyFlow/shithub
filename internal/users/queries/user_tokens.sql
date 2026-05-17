-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertUserToken :one
-- COALESCE on the ip_allowlist param so callers that don't supply it
-- (test helpers + the pre-PRO-EXT01-11a handler path) get the empty-
-- array default rather than a NOT NULL constraint violation.
-- repo_id is nullable — NULL means "no binding".
-- source defaults to 'user_created' so existing call sites stay
-- source-naive; the device-flow Exchange path (internal/auth/devicecode)
-- passes 'oauth_device' explicitly. The empty-string sentinel maps to
-- the column DEFAULT via NULLIF + COALESCE so a caller that hasn't yet
-- been updated to set Source compiles and behaves correctly.
INSERT INTO user_tokens (user_id, name, token_hash, token_prefix, scopes, expires_at, ip_allowlist, repo_id, source)
VALUES ($1, $2, $3, $4, $5, $6,
        COALESCE(sqlc.arg(ip_allowlist)::text[], '{}'::text[]),
        $7,
        COALESCE(NULLIF(sqlc.arg(source)::text, ''), 'user_created'))
RETURNING id, user_id, name, token_hash, token_prefix, scopes,
          expires_at, last_used_at, last_used_ip, revoked_at, created_at,
          ip_allowlist, repo_id, source;

-- name: UpdateUserTokenIPAllowlist :exec
-- Scoped by user_id so a hijacked handler can't reach into someone
-- else's token row. Pro-feature gate is enforced in the handler;
-- this query is dumb plumbing.
UPDATE user_tokens
SET ip_allowlist = $3
WHERE id = $1 AND user_id = $2;

-- name: ListUserTokens :many
SELECT id, user_id, name, token_hash, token_prefix, scopes,
       expires_at, last_used_at, last_used_ip, revoked_at, created_at,
       ip_allowlist, repo_id, source
FROM user_tokens
WHERE user_id = $1
ORDER BY revoked_at IS NOT NULL, created_at DESC;

-- name: CountActiveUserTokens :one
SELECT count(*) FROM user_tokens
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: RevokeUserToken :execrows
-- Scoped revoke: caller must pass owning user_id so a hijacked handler
-- can never revoke tokens it doesn't own. No-op on already-revoked rows.
UPDATE user_tokens
SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: RevokeAllUserTokens :exec
-- Used by user suspension to revoke every active token in one statement.
UPDATE user_tokens
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: GetUserTokenByHash :one
-- Hot path for the auth middleware. token_hash is UNIQUE; returns at
-- most one row. Caller MUST also check revoked_at IS NULL and
-- expires_at handling. repo_id (PRO-EXT01-11b) is included so the
-- middleware can propagate the binding to downstream route helpers.
SELECT id, user_id, name, token_hash, token_prefix, scopes,
       expires_at, last_used_at, last_used_ip, revoked_at, created_at,
       ip_allowlist, repo_id
FROM user_tokens
WHERE token_hash = $1;

-- name: TouchUserTokenLastUsed :exec
UPDATE user_tokens
SET last_used_at = now(),
    last_used_ip = $2
WHERE id = $1;
