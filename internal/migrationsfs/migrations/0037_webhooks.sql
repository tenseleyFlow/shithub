-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S33 — Webhooks. Two tables:
--
--   * webhooks         — operator-configured subscriptions. Owner is a
--                        repo OR an org (org-owned repos can carry
--                        their own org-level webhooks too, fanned out
--                        on every repo event in the org).
--   * webhook_deliveries — one row per delivery attempt. Pending /
--                        retry rows are picked up by the deliver
--                        worker. Successful and terminally-failed rows
--                        are kept for the redelivery UI; the purge
--                        cron prunes them after retention.
--
-- Secrets at rest:
--   - `secret_ciphertext` + `secret_nonce` are wrapped with
--     `internal/auth/secretbox` (chacha20poly1305 AEAD; 32-byte key
--     from config). The key is the same TOTP key for now — see
--     `webhooks.md` for rotation procedure.
--
-- Auto-disable:
--   - `consecutive_failures` increments on terminal failure, resets on
--     success. When it crosses `auto_disable_threshold` the deliverer
--     sets `disabled_at` + `disabled_reason` and surfaces a warning to
--     repo/org admins.

-- +goose Up

CREATE TYPE webhook_owner_kind AS ENUM ('repo', 'org');
CREATE TYPE webhook_content_type AS ENUM ('json', 'form');
CREATE TYPE webhook_delivery_status AS ENUM (
    'pending', 'succeeded', 'failed_retry', 'failed_permanent'
);

CREATE TABLE webhooks (
    id                       bigserial            PRIMARY KEY,
    owner_kind               webhook_owner_kind   NOT NULL,
    owner_id                 bigint               NOT NULL,
    url                      text                 NOT NULL,
    content_type             webhook_content_type NOT NULL DEFAULT 'json',
    -- Subscribed event kinds. Empty set means "all"; explicit subset
    -- restricts the fan-out match. Rows that change shape (e.g. push
    -- with no commits) still fire — filter expressions are post-MVP.
    events                   text[]               NOT NULL DEFAULT ARRAY[]::text[],
    secret_ciphertext        bytea                NOT NULL,
    secret_nonce             bytea                NOT NULL,
    active                   boolean              NOT NULL DEFAULT true,
    -- Per-webhook SSL verification override. Default keeps verification
    -- on; self-hosters with internal CAs can flip it (UI surfaces a
    -- loud warning). Enforcement ships with the deliverer; the column
    -- is wired up day-1 so config doesn't churn later.
    ssl_verification         boolean              NOT NULL DEFAULT true,
    consecutive_failures     int                  NOT NULL DEFAULT 0,
    auto_disable_threshold   int                  NOT NULL DEFAULT 50,
    disabled_at              timestamptz,
    disabled_reason          text,
    last_success_at          timestamptz,
    last_failure_at          timestamptz,
    created_by_user_id       bigint               REFERENCES users(id) ON DELETE SET NULL,
    created_at               timestamptz          NOT NULL DEFAULT now(),
    updated_at               timestamptz          NOT NULL DEFAULT now(),

    CONSTRAINT webhooks_url_length CHECK (char_length(url) BETWEEN 1 AND 2048),
    CONSTRAINT webhooks_threshold_positive CHECK (auto_disable_threshold > 0)
);

CREATE INDEX webhooks_owner_idx ON webhooks (owner_kind, owner_id);
CREATE INDEX webhooks_active_idx ON webhooks (owner_kind, owner_id) WHERE active = true AND disabled_at IS NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON webhooks
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE webhook_deliveries (
    id                  bigserial               PRIMARY KEY,
    webhook_id          bigint                  NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    -- event_kind mirrors `domain_events.kind`. event_id points at the
    -- triggering domain_events row; NULL for synthetic deliveries
    -- (ping, redelivery from a manual UI click).
    event_kind          text                    NOT NULL,
    event_id            bigint,
    delivery_uuid       uuid                    NOT NULL DEFAULT gen_random_uuid(),
    payload             jsonb                   NOT NULL,
    request_headers     jsonb                   NOT NULL DEFAULT '{}'::jsonb,
    request_body        bytea                   NOT NULL DEFAULT ''::bytea,
    response_status     int,
    response_headers    jsonb,
    response_body       bytea,
    response_truncated  boolean                 NOT NULL DEFAULT false,
    started_at          timestamptz             NOT NULL DEFAULT now(),
    completed_at        timestamptz,
    attempt             int                     NOT NULL DEFAULT 1,
    max_attempts        int                     NOT NULL DEFAULT 8,
    next_retry_at       timestamptz,
    status              webhook_delivery_status NOT NULL DEFAULT 'pending',
    -- idempotency_key is sha256(payload + webhook_id + event_id);
    -- subscribers can dedupe across retries with it.
    idempotency_key     text                    NOT NULL,
    error_summary       text,
    -- Set when the delivery was created via UI redelivery; points at
    -- the original delivery for audit. Nullable for first attempts.
    redeliver_of        bigint                  REFERENCES webhook_deliveries(id) ON DELETE SET NULL,

    CONSTRAINT webhook_deliveries_event_kind_length CHECK (char_length(event_kind) BETWEEN 1 AND 64),
    CONSTRAINT webhook_deliveries_attempt_positive CHECK (attempt >= 1),
    CONSTRAINT webhook_deliveries_max_attempts_positive CHECK (max_attempts >= 1)
);

-- Deliver-loop hot path: pending or retry-ready, due now.
CREATE INDEX webhook_deliveries_pending_due_idx
    ON webhook_deliveries (next_retry_at)
    WHERE status IN ('pending', 'failed_retry');

-- Per-webhook delivery list (settings UI).
CREATE INDEX webhook_deliveries_webhook_started_idx
    ON webhook_deliveries (webhook_id, started_at DESC);

-- +goose Down
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhooks;
DROP TYPE IF EXISTS webhook_delivery_status;
DROP TYPE IF EXISTS webhook_content_type;
DROP TYPE IF EXISTS webhook_owner_kind;
