-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertRecoveryCode :exec
INSERT INTO user_recovery_codes (user_id, code_hash) VALUES ($1, $2);

-- name: ConsumeRecoveryCode :execrows
-- Atomically marks a code as used iff it exists for the user, matches the
-- supplied hash, and isn't already used. Rows-affected==1 means accepted;
-- 0 means rejected.
UPDATE user_recovery_codes
SET used_at = now()
WHERE user_id = $1 AND code_hash = $2 AND used_at IS NULL;

-- name: DeleteUserRecoveryCodes :exec
DELETE FROM user_recovery_codes WHERE user_id = $1;

-- name: CountUnusedRecoveryCodes :one
SELECT count(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL;
