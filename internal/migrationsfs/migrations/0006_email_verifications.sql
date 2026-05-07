-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Email verification tokens. Same pattern as password_resets but keyed
-- to a specific user_email row (a user with multiple emails verifies
-- each one separately). TTL 24h. Single-use via used_at.

-- +goose Up
CREATE TABLE email_verifications (
    id            bigserial   PRIMARY KEY,
    user_email_id bigint      NOT NULL REFERENCES user_emails(id) ON DELETE CASCADE,
    token_hash    bytea       NOT NULL UNIQUE,
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX email_verifications_email_id_idx ON email_verifications (user_email_id);
CREATE INDEX email_verifications_expires_at_idx ON email_verifications (expires_at);

-- +goose Down
DROP TABLE IF EXISTS email_verifications;
