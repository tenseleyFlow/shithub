-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S34 — Site admin. Two pieces:
--
--  1. users.is_site_admin — flips a user into the site-admin role.
--     The first admin is bootstrapped via the
--     `shithubd admin bootstrap-admin <username>` CLI subcommand;
--     subsequent toggles happen in the /admin/users/{id} UI.
--
--  2. transactional_email_log — sibling to S29's notification_email_log.
--     Captures auth resets, 2FA notices, transfer-offer emails, etc.
--     so the admin email-queue view can surface delivery health for
--     non-notification mail. We log envelope metadata and delivery
--     status; the message body is intentionally NOT persisted (PII
--     containment + retention story is simpler that way).
--
-- Impersonation state lives in the session cookie itself (S02 cookie
-- store, not a DB row), so this migration doesn't touch sessions.

-- +goose Up
ALTER TABLE users ADD COLUMN is_site_admin boolean NOT NULL DEFAULT false;
CREATE INDEX users_site_admin_idx ON users (is_site_admin) WHERE is_site_admin = true;

CREATE TYPE transactional_email_status AS ENUM (
    'queued', 'sent', 'soft_bounced', 'hard_bounced', 'dropped'
);

CREATE TABLE transactional_email_log (
    id              bigserial                  PRIMARY KEY,
    -- recipient_user_id is nullable so unsubscribed-recipient flows
    -- (e.g. transfer offer to a username that hasn't signed up) still
    -- get logged for the audit trail.
    recipient_user_id  bigint                  REFERENCES users(id) ON DELETE SET NULL,
    recipient_email    citext                  NOT NULL,
    -- kind groups events for the admin filter: 'password_reset',
    -- 'verify_email', 'admin_cleared_2fa', 'transfer_offer', etc.
    kind            text                       NOT NULL,
    subject         text                       NOT NULL,
    -- Provider-supplied id when the backend exposes one (Postmark
    -- MessageID); empty string for stdout/SMTP backends.
    provider_id     text                       NOT NULL DEFAULT '',
    status          transactional_email_status NOT NULL DEFAULT 'queued',
    -- error_summary captures the failure reason for the bounce/drop
    -- triage path. Cleared on successful re-send.
    error_summary   text,
    sent_at         timestamptz                NOT NULL DEFAULT now(),
    delivered_at    timestamptz,

    CONSTRAINT transactional_email_log_kind_length CHECK (char_length(kind) BETWEEN 1 AND 64),
    CONSTRAINT transactional_email_log_subject_length CHECK (char_length(subject) BETWEEN 1 AND 500)
);

-- Admin "recent failures" surface: scan failed rows in time order.
CREATE INDEX transactional_email_log_status_idx
    ON transactional_email_log (status, sent_at DESC)
    WHERE status IN ('soft_bounced', 'hard_bounced', 'dropped');

-- Admin filter by kind + recency.
CREATE INDEX transactional_email_log_kind_sent_idx
    ON transactional_email_log (kind, sent_at DESC);

-- +goose Down
DROP TABLE IF EXISTS transactional_email_log;
DROP TYPE IF EXISTS transactional_email_status;
DROP INDEX IF EXISTS users_site_admin_idx;
ALTER TABLE users DROP COLUMN IF EXISTS is_site_admin;
