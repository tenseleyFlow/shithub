-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertUserGPGKey :one
-- Inserts a parsed primary GPG key. Subkeys land in user_gpg_subkeys
-- in the same transaction (see InsertUserGPGSubkey). expires_at is
-- nullable; many keys have no expiration. revoked_at stays NULL on
-- insert; soft-delete sets it.
INSERT INTO user_gpg_keys (
    user_id, name, fingerprint, key_id, armored,
    can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
    uids, subkeys, primary_algo, expires_at
)
VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, $12, $13, $14
)
RETURNING id, user_id, name, fingerprint, key_id, armored,
          can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
          uids, subkeys, primary_algo,
          created_at, last_used_at, revoked_at, expires_at;

-- name: ListUserGPGKeys :many
-- Paginated list for the REST surface; HTML settings page reuses with
-- a generous limit and no offset.
SELECT id, user_id, name, fingerprint, key_id, armored,
       can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
       uids, subkeys, primary_algo,
       created_at, last_used_at, revoked_at, expires_at
FROM user_gpg_keys
WHERE user_id = $1 AND revoked_at IS NULL
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: CountUserGPGKeys :one
-- Excludes revoked rows so the per-user cap (100) counts live keys.
SELECT count(*) FROM user_gpg_keys WHERE user_id = $1 AND revoked_at IS NULL;

-- name: GetUserGPGKey :one
-- Scoped single-key lookup for REST GET-by-id. user_id filter prevents
-- cross-user reads (existence-leak-safe: returns no row if the id
-- belongs to another user).
SELECT id, user_id, name, fingerprint, key_id, armored,
       can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
       uids, subkeys, primary_algo,
       created_at, last_used_at, revoked_at, expires_at
FROM user_gpg_keys
WHERE id = $1 AND user_id = $2;

-- name: GetUserGPGKeyForVerification :one
-- Non-user-scoped lookup used by the verification path. Unlike
-- GetUserGPGKey this query does NOT filter on user_id — the caller
-- already validated the subkey resolution and needs the parent
-- record's user_id to drive the email cross-check. Includes revoked
-- rows so historical commit verifications can still resolve their
-- signer attribution.
SELECT id, user_id, name, fingerprint, key_id, armored,
       can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
       uids, subkeys, primary_algo,
       created_at, last_used_at, revoked_at, expires_at
FROM user_gpg_keys
WHERE id = $1;

-- name: GetUserGPGKeyByFingerprint :one
-- Uniqueness probe used by the add path to surface a friendly
-- "this key is already registered" error before the unique index
-- violation. Returns any row matching the fingerprint regardless of
-- which user owns it (global uniqueness is the contract).
SELECT id, user_id, name, fingerprint, key_id, armored,
       can_sign, can_encrypt_comms, can_encrypt_storage, can_certify, can_authenticate,
       uids, subkeys, primary_algo,
       created_at, last_used_at, revoked_at, expires_at
FROM user_gpg_keys
WHERE fingerprint = $1 AND revoked_at IS NULL;

-- name: SoftDeleteUserGPGKey :execrows
-- Scoped soft-delete: stamps revoked_at, preserves the row for audit
-- continuity. Returns the number of rows affected so the handler can
-- distinguish "not found" from "deleted" without a follow-up query.
UPDATE user_gpg_keys
SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: TouchUserGPGKeyLastUsed :exec
-- Best-effort last-used stamp called from the verification path when
-- a signature successfully resolves to this key. No timeout / error
-- propagation; the caller fires-and-forgets via a goroutine.
UPDATE user_gpg_keys SET last_used_at = now() WHERE id = $1;
