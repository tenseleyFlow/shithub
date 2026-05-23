-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertUserTOTP :one
-- Inserts a new pending TOTP row, or replaces an existing pending row for
-- the same user. Confirmed rows are NOT replaced — disable+regenerate
-- must go through the dedicated query.
INSERT INTO user_totp (user_id, secret_encrypted, secret_nonce)
VALUES ($1, $2, $3)
ON CONFLICT (user_id) DO UPDATE
SET secret_encrypted = EXCLUDED.secret_encrypted,
    secret_nonce     = EXCLUDED.secret_nonce,
    confirmed_at     = NULL,
    last_used_counter = 0
WHERE user_totp.confirmed_at IS NULL
RETURNING id, user_id, secret_encrypted, secret_nonce, confirmed_at,
          last_used_counter, created_at, updated_at;

-- name: GetUserTOTP :one
SELECT id, user_id, secret_encrypted, secret_nonce, confirmed_at,
       last_used_counter, created_at, updated_at
FROM user_totp
WHERE user_id = $1;

-- name: HasConfirmedUserTOTP :one
SELECT EXISTS (
    SELECT 1
    FROM user_totp
    WHERE user_id = $1
      AND confirmed_at IS NOT NULL
);

-- name: ConfirmUserTOTP :execrows
-- Sets confirmed_at on a pending row. Returns the number of rows updated;
-- callers MUST check this to handle the parallel-enrollment race
-- (only one of two concurrent confirms wins).
UPDATE user_totp
SET confirmed_at      = now(),
    last_used_counter = $2
WHERE user_id = $1 AND confirmed_at IS NULL;

-- name: BumpTOTPCounter :execrows
-- Atomically advances last_used_counter only when the proposed counter is
-- strictly greater. Returns rows affected — 0 means a replay attempt and
-- the caller should reject the code.
UPDATE user_totp
SET last_used_counter = $2
WHERE user_id = $1 AND $2::bigint > last_used_counter;

-- name: DeleteUserTOTP :exec
DELETE FROM user_totp WHERE user_id = $1;
