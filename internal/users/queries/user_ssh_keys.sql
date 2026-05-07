-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertUserSSHKey :one
INSERT INTO user_ssh_keys (user_id, title, fingerprint_sha256, key_type, key_bits, public_key)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, title, fingerprint_sha256, key_type, key_bits, public_key,
          last_used_at, last_used_ip, created_at;

-- name: ListUserSSHKeys :many
SELECT id, user_id, title, fingerprint_sha256, key_type, key_bits, public_key,
       last_used_at, last_used_ip, created_at
FROM user_ssh_keys
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: CountUserSSHKeys :one
SELECT count(*) FROM user_ssh_keys WHERE user_id = $1;

-- name: DeleteUserSSHKey :execrows
-- Scoped delete: caller must pass the owning user_id so a hijacked
-- handler can never delete keys it doesn't own.
DELETE FROM user_ssh_keys WHERE id = $1 AND user_id = $2;

-- name: GetUserSSHKeyByFingerprint :one
-- Hot path for sshd's AuthorizedKeysCommand. Index lookup via the UNIQUE
-- index on fingerprint_sha256.
SELECT id, user_id, title, fingerprint_sha256, key_type, key_bits, public_key,
       last_used_at, last_used_ip, created_at
FROM user_ssh_keys
WHERE fingerprint_sha256 = $1;

-- name: TouchSSHKeyLastUsed :exec
UPDATE user_ssh_keys
SET last_used_at = now(),
    last_used_ip = $2
WHERE id = $1;
