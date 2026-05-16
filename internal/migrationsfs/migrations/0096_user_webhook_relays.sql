-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-13a: per-user webhook relay.
--
-- A relay is an inbound endpoint a Pro user creates so they can paste
-- shithub's relay URL into an upstream service's webhook config.
-- Inbound POSTs are stored as pending delivery rows (one per
-- configured destination URL) and the deliver worker fans them out
-- with HMAC signing.
--
-- Tables:
--   user_webhook_relays         — one row per relay endpoint.
--   webhook_relay_deliveries    — one row per outbound attempt.
--
-- Tokens are SHA-256 hashed at rest (mirrors PAT in
-- internal/auth/pat). Display prefix is the first ~8 chars; full raw
-- never round-trips after creation. The HMAC secret each destination
-- is signed with is AEAD-encrypted with the same webhook AEAD box the
-- repo webhook subsystem uses (cfg.Webhook.AEADKey).
--
-- Destinations are stored as a JSONB array on the relay row rather
-- than a third table — a relay is small (cap enforced at PR 13c's
-- create handler), destinations change atomically with the relay, and
-- the join cost of a third table buys nothing for the receive path.
-- The delivery row snapshots the URL it targeted so a destination
-- removal doesn't orphan in-flight retries.
--
-- Body cap at the receiver is 1 MiB (enforced in the handler, not the
-- column type). The payload_bytes column is bytea with no length cap
-- so a future raise of the receiver cap doesn't need a migration.
--
-- Down-migration note: drops both tables and the enum type. Any
-- pending deliveries are unrecoverable — operators rolling back
-- should drain the queue first.

-- +goose Up
CREATE TABLE user_webhook_relays (
    id                     BIGSERIAL PRIMARY KEY,
    user_id                BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                   TEXT NOT NULL,
    token_hash             BYTEA NOT NULL UNIQUE,
    token_prefix           TEXT NOT NULL,
    hmac_secret_ciphertext BYTEA NOT NULL,
    hmac_secret_nonce      BYTEA NOT NULL,
    destinations           JSONB NOT NULL DEFAULT '[]'::JSONB,
    disabled_at            TIMESTAMPTZ,
    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX user_webhook_relays_user_id_idx
    ON user_webhook_relays (user_id);

CREATE TYPE webhook_relay_delivery_status AS ENUM (
    'pending', 'succeeded', 'failed_retry', 'failed_permanent'
);

CREATE TABLE webhook_relay_deliveries (
    id               BIGSERIAL PRIMARY KEY,
    relay_id         BIGINT NOT NULL REFERENCES user_webhook_relays(id) ON DELETE CASCADE,
    destination_url  TEXT NOT NULL,
    status           webhook_relay_delivery_status NOT NULL DEFAULT 'pending',
    attempt          INT NOT NULL DEFAULT 0,
    max_attempts     INT NOT NULL DEFAULT 8,
    next_attempt_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload_bytes    BYTEA NOT NULL,
    request_id       TEXT NOT NULL,
    last_status_code INT,
    last_error       TEXT,
    delivered_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Partial index: the worker scans only rows that may still need work.
-- Terminal rows (succeeded / failed_permanent) live in the table for
-- audit but never appear in due-row sweeps.
CREATE INDEX webhook_relay_deliveries_due_idx
    ON webhook_relay_deliveries (status, next_attempt_at)
    WHERE status IN ('pending', 'failed_retry');

CREATE INDEX webhook_relay_deliveries_relay_id_idx
    ON webhook_relay_deliveries (relay_id);

-- +goose Down
DROP TABLE IF EXISTS webhook_relay_deliveries;
DROP TYPE IF EXISTS webhook_relay_delivery_status;
DROP TABLE IF EXISTS user_webhook_relays;
