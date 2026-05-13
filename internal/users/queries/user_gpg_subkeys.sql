-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertUserGPGSubkey :one
-- One row per subkey of a primary key. Always inserted in the same
-- transaction as the parent InsertUserGPGKey so the verification
-- hot path's fingerprint lookup is consistent with the REST nested
-- shape.
INSERT INTO user_gpg_subkeys (
    gpg_key_id, fingerprint, key_id,
    can_sign, can_encrypt_comms, can_encrypt_storage, can_certify,
    expires_at
)
VALUES (
    $1, $2, $3,
    $4, $5, $6, $7,
    $8
)
RETURNING id, gpg_key_id, fingerprint, key_id,
          can_sign, can_encrypt_comms, can_encrypt_storage, can_certify,
          expires_at, revoked_at, created_at;

-- name: GetUserGPGSubkeyByFingerprint :one
-- Hot path for commit/tag signature verification. The signature
-- packet carries the signing subkey's fingerprint; this query
-- resolves it back to the primary key (and via FK to the user).
-- Index lookup via the partial unique index.
SELECT id, gpg_key_id, fingerprint, key_id,
       can_sign, can_encrypt_comms, can_encrypt_storage, can_certify,
       expires_at, revoked_at, created_at
FROM user_gpg_subkeys
WHERE fingerprint = $1 AND revoked_at IS NULL;

-- name: ListSubkeysForGPGKey :many
-- Reads all live subkeys for one primary; used when invalidating the
-- verification cache on primary soft-delete (every dependent subkey
-- needs its cache rows stamped invalidated too).
SELECT id, gpg_key_id, fingerprint, key_id,
       can_sign, can_encrypt_comms, can_encrypt_storage, can_certify,
       expires_at, revoked_at, created_at
FROM user_gpg_subkeys
WHERE gpg_key_id = $1
ORDER BY id;

-- name: SoftDeleteSubkeysForGPGKey :exec
-- Stamps revoked_at on every live subkey of a primary. Called in the
-- same transaction as SoftDeleteUserGPGKey so the partial unique index
-- frees up the fingerprint for re-upload if the user rotates.
UPDATE user_gpg_subkeys
SET revoked_at = now()
WHERE gpg_key_id = $1 AND revoked_at IS NULL;
