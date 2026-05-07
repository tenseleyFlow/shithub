-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateEmailVerification :one
INSERT INTO email_verifications (user_email_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING id, user_email_id, token_hash, expires_at, used_at, created_at;

-- name: GetEmailVerificationByTokenHash :one
SELECT id, user_email_id, token_hash, expires_at, used_at, created_at
FROM email_verifications
WHERE token_hash = $1;

-- name: ConsumeEmailVerification :exec
UPDATE email_verifications
SET used_at = now()
WHERE id = $1 AND used_at IS NULL;

-- name: DeleteExpiredEmailVerifications :exec
DELETE FROM email_verifications WHERE expires_at < now();
