-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- TOTP (time-based one-time password) secrets, one per user. The secret
-- is encrypted at rest with chacha20poly1305 (key from config: secrets.totp_key).
-- secret_nonce is the per-row 12-byte AEAD nonce; never reused for the same key.
--
-- last_used_counter is the highest TOTP step counter we've accepted; codes
-- whose counter is <= this value are rejected to prevent replay (RFC 6238 §5.2).
-- confirmed_at is NULL during enrollment until the user proves possession of
-- the authenticator with a fresh code.

-- +goose Up
CREATE TABLE user_totp (
    id                bigserial   PRIMARY KEY,
    user_id           bigint      NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
    secret_encrypted  bytea       NOT NULL,
    secret_nonce      bytea       NOT NULL,
    confirmed_at      timestamptz,
    last_used_counter bigint      NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_totp_nonce_size CHECK (octet_length(secret_nonce) = 12)
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON user_totp
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS user_totp;
