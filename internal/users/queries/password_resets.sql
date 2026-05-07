-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreatePasswordReset :one
INSERT INTO password_resets (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING id, user_id, token_hash, expires_at, used_at, created_at;

-- name: GetPasswordResetByTokenHash :one
SELECT id, user_id, token_hash, expires_at, used_at, created_at
FROM password_resets
WHERE token_hash = $1;

-- name: ConsumePasswordReset :exec
UPDATE password_resets
SET used_at = now()
WHERE id = $1 AND used_at IS NULL;

-- name: DeleteExpiredPasswordResets :exec
DELETE FROM password_resets WHERE expires_at < now();
