-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- RFC 8628 (OAuth 2.0 Device Authorization Grant) state. Each row is a
-- single in-flight authorization request from a CLI / non-browser
-- client; the row's lifetime is bounded by `expires_at` (default 15
-- minutes from issue).
--
-- device_code_hash holds sha256(raw_device_code); the raw value is
-- returned to the client exactly once and never stored. user_code is
-- the short, human-typeable identifier shown on the CLI ("ABCD-EFGH")
-- and entered by the user on the verification page; we store it
-- plaintext because it's intentionally low-entropy and the row is
-- garbage-collected on expiry.
--
-- A row's terminal state is one of:
--   * approved_at IS NOT NULL → exchange yields an access token (a row
--     in user_tokens, joined via issued_token_id).
--   * denied_at IS NOT NULL → /login/oauth/access_token returns
--     access_denied; row stays until expires_at for forensics.
--   * expires_at < now() with no approval/denial → invalid_grant on
--     exchange; same forensics window.
--
-- last_polled_at + interval_seconds back the `slow_down` enforcement
-- that RFC 8628 §3.5 mandates so misbehaving clients can't busy-poll.

-- +goose Up
CREATE TABLE device_authorizations (
    id                 bigserial   PRIMARY KEY,
    device_code_hash   bytea       NOT NULL UNIQUE,
    user_code          text        NOT NULL UNIQUE,
    client_id          text        NOT NULL,
    scopes             text[]      NOT NULL DEFAULT ARRAY[]::text[],
    user_id            bigint      REFERENCES users(id) ON DELETE CASCADE,
    approved_at        timestamptz,
    denied_at          timestamptz,
    issued_token_id    bigint      REFERENCES user_tokens(id) ON DELETE SET NULL,
    interval_seconds   integer     NOT NULL DEFAULT 5,
    expires_at         timestamptz NOT NULL,
    last_polled_at     timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT device_authorizations_hash_size CHECK (octet_length(device_code_hash) = 32),
    CONSTRAINT device_authorizations_user_code_length CHECK (char_length(user_code) BETWEEN 4 AND 32)
);

CREATE INDEX device_authorizations_expires_at_idx
    ON device_authorizations (expires_at);

-- Pending-only lookup index — user_code is meaningful only while the row
-- is awaiting approval; once approved/denied/expired the index can be
-- skipped on the user-entry path.
CREATE INDEX device_authorizations_pending_user_code_idx
    ON device_authorizations (user_code)
    WHERE approved_at IS NULL AND denied_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS device_authorizations;
