-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateUserEmail :one
INSERT INTO user_emails (user_id, email, is_primary, verified, verification_token_hash, verification_sent_at)
VALUES ($1, $2, $3, $4, $5, CASE WHEN $5::bytea IS NULL THEN NULL ELSE now() END)
RETURNING id, user_id, email, is_primary, verified, verification_token_hash,
          verification_sent_at, verified_at, created_at;

-- name: GetUserEmailByAddress :one
SELECT id, user_id, email, is_primary, verified, verification_token_hash,
       verification_sent_at, verified_at, created_at
FROM user_emails
WHERE email = $1;

-- name: GetUserEmailByID :one
SELECT id, user_id, email, is_primary, verified, verification_token_hash,
       verification_sent_at, verified_at, created_at
FROM user_emails
WHERE id = $1;

-- name: ListUserEmailsForUser :many
SELECT id, user_id, email, is_primary, verified, verification_token_hash,
       verification_sent_at, verified_at, created_at
FROM user_emails
WHERE user_id = $1
ORDER BY is_primary DESC, created_at;

-- name: MarkUserEmailVerified :exec
UPDATE user_emails
SET verified                = true,
    verified_at             = now(),
    verification_token_hash = NULL
WHERE id = $1;

-- name: SetVerificationToken :exec
UPDATE user_emails
SET verification_token_hash = $2,
    verification_sent_at    = now()
WHERE id = $1;

-- name: GetUserEmailByVerificationHash :one
SELECT id, user_id, email, is_primary, verified, verification_token_hash,
       verification_sent_at, verified_at, created_at
FROM user_emails
WHERE verification_token_hash = $1;

-- name: SetUserEmailPrimary :exec
-- Atomically unset the existing primary and set the supplied row as
-- primary. Caller MUST have already verified the row belongs to the
-- user and is verified.
UPDATE user_emails SET is_primary = (id = $2) WHERE user_id = $1;

-- name: DeleteUserEmail :execrows
-- Scoped delete: caller must pass owning user_id. Refuses to delete
-- the primary email (UI must guide the user to set a different primary first).
DELETE FROM user_emails
WHERE id = $1 AND user_id = $2 AND is_primary = false;

-- name: CountVerifiedUserEmails :one
SELECT count(*) FROM user_emails WHERE user_id = $1 AND verified = true;
