-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertUserToken :one
INSERT INTO user_tokens (user_id, name, token_hash, token_prefix, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, name, token_hash, token_prefix, scopes,
          expires_at, last_used_at, last_used_ip, revoked_at, created_at;

-- name: ListUserTokens :many
SELECT id, user_id, name, token_hash, token_prefix, scopes,
       expires_at, last_used_at, last_used_ip, revoked_at, created_at
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
-- expires_at handling.
SELECT id, user_id, name, token_hash, token_prefix, scopes,
       expires_at, last_used_at, last_used_ip, revoked_at, created_at
FROM user_tokens
WHERE token_hash = $1;

-- name: TouchUserTokenLastUsed :exec
UPDATE user_tokens
SET last_used_at = now(),
    last_used_ip = $2
WHERE id = $1;
