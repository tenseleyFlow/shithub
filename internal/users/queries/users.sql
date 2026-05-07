-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateUser :one
INSERT INTO users (username, display_name, password_hash)
VALUES ($1, $2, $3)
RETURNING id, username, display_name, primary_email_id, password_hash, password_algo,
          password_updated_at, email_verified, last_login_at, suspended_at,
          suspended_reason, deleted_at, created_at, updated_at;

-- name: GetUserByID :one
SELECT id, username, display_name, primary_email_id, password_hash, password_algo,
       password_updated_at, email_verified, last_login_at, suspended_at,
       suspended_reason, deleted_at, created_at, updated_at
FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserByUsername :one
SELECT id, username, display_name, primary_email_id, password_hash, password_algo,
       password_updated_at, email_verified, last_login_at, suspended_at,
       suspended_reason, deleted_at, created_at, updated_at
FROM users
WHERE username = $1 AND deleted_at IS NULL;

-- name: UpdateUserPassword :exec
UPDATE users
SET password_hash       = $2,
    password_algo       = $3,
    password_updated_at = now()
WHERE id = $1;

-- name: LinkUserPrimaryEmail :exec
-- Sets the FK only. Does NOT flip users.email_verified — that happens via
-- MarkUserEmailPrimaryVerified after the user clicks the verification link.
UPDATE users
SET primary_email_id = $2
WHERE id = $1;

-- name: MarkUserEmailPrimaryVerified :exec
-- Called after MarkUserEmailVerified for the primary email, to flip the
-- denormalized users.email_verified flag.
UPDATE users
SET email_verified = true
WHERE id = $1;

-- name: TouchUserLastLogin :exec
UPDATE users
SET last_login_at = now()
WHERE id = $1;

-- name: SuspendUser :exec
UPDATE users
SET suspended_at     = now(),
    suspended_reason = $2
WHERE id = $1;

-- name: SoftDeleteUser :exec
UPDATE users
SET deleted_at = now()
WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users WHERE deleted_at IS NULL;
