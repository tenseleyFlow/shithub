-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateRelay :one
INSERT INTO user_webhook_relays (
    user_id, name, token_hash, token_prefix,
    hmac_secret_ciphertext, hmac_secret_nonce, destinations
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, user_id, name, token_hash, token_prefix,
          hmac_secret_ciphertext, hmac_secret_nonce, destinations,
          disabled_at, created_at, updated_at;

-- name: GetRelayByTokenHash :one
SELECT id, user_id, name, token_hash, token_prefix,
       hmac_secret_ciphertext, hmac_secret_nonce, destinations,
       disabled_at, created_at, updated_at
FROM user_webhook_relays
WHERE token_hash = $1;

-- name: GetRelayByID :one
SELECT id, user_id, name, token_hash, token_prefix,
       hmac_secret_ciphertext, hmac_secret_nonce, destinations,
       disabled_at, created_at, updated_at
FROM user_webhook_relays
WHERE id = $1;

-- name: ListRelaysForUser :many
SELECT id, user_id, name, token_hash, token_prefix,
       hmac_secret_ciphertext, hmac_secret_nonce, destinations,
       disabled_at, created_at, updated_at
FROM user_webhook_relays
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: DisableRelay :exec
UPDATE user_webhook_relays
SET disabled_at = now(), updated_at = now()
WHERE id = $1;

-- name: DeleteRelay :exec
DELETE FROM user_webhook_relays WHERE id = $1;
