-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateUser :one
INSERT INTO users (username, display_name, password_hash)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetUserByID :one
SELECT *
FROM users
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListUsersByIDs :many
-- Batch lookup for participant rendering on issue/PR views and other
-- multi-user surfaces. Empty / NULL entries in the input array are
-- silently filtered. PRO-EXT_SR2-12 (audit H5).
SELECT *
FROM users
WHERE deleted_at IS NULL
  AND id = ANY($1::bigint[]);

-- name: GetUserByUsername :one
SELECT *
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

-- name: UnsuspendUser :exec
-- Clears the suspended state. Mirrors SuspendUser; used by the
-- /admin/users/{id}/unsuspend handler. Replaces an inline UPDATE
-- in admin/users.go (SR2 M2).
UPDATE users
SET suspended_at     = NULL,
    suspended_reason = NULL
WHERE id = $1;

-- name: SoftDeleteUser :exec
UPDATE users
SET deleted_at = now()
WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users WHERE deleted_at IS NULL;

-- name: UpdateUserProfile :exec
UPDATE users
SET display_name = $2,
    bio          = $3,
    location     = $4,
    website      = $5,
    company      = $6,
    pronouns     = $7
WHERE id = $1;

-- name: UpdateUserAvatarKey :exec
UPDATE users
SET avatar_object_key = $2
WHERE id = $1;

-- name: UpdateUserProfileVanity :exec
-- PRO-EXT01-04: writes the Pro-tier vanity settings (accent color +
-- pin layout). Handler guards the write behind FeatureProfileVanity;
-- inputs are pre-validated against the column CHECK constraints so
-- this query trusts its arguments.
UPDATE users
SET profile_accent_hex = $2,
    profile_layout     = $3
WHERE id = $1;

-- name: RenameUser :exec
-- Wrapped by the username-change flow inside a tx that also writes
-- username_redirects, so the old name becomes a redirect target atomically.
UPDATE users
SET username = $2
WHERE id = $1;

-- name: CountRecentUsernameChanges :one
-- Drives the 3-changes-per-60d cap.
SELECT count(*) FROM username_redirects
WHERE user_id = $1 AND changed_at > $2;

-- name: UpdateUserTheme :exec
UPDATE users SET theme = $2 WHERE id = $1;

-- name: UpdateUserPrivateContributions :exec
UPDATE users SET include_private_contributions = $2 WHERE id = $1;

-- name: BumpUserSessionEpoch :exec
UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1;

-- name: GetUserSessionEpoch :one
SELECT session_epoch FROM users WHERE id = $1;

-- name: RestoreUserAccount :exec
-- Clears deleted_at; called when a user logs in within the 14-day grace
-- window. The login handler enforces the window check before calling.
UPDATE users SET deleted_at = NULL WHERE id = $1;

-- name: GetUserIncludingDeleted :one
-- Like GetUserByID but returns the row even when deleted_at IS NOT NULL.
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsernameIncludingDeleted :one
SELECT * FROM users WHERE username = $1;
