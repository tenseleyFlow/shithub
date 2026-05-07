-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- User email addresses. Users can have multiple emails; exactly one is
-- the primary at any time (enforced by a partial unique index).
-- email is citext + globally unique across all users (an email address
-- can belong to at most one shithub account).

-- +goose Up
CREATE TABLE user_emails (
    id                       bigserial   PRIMARY KEY,
    user_id                  bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email                    citext      NOT NULL UNIQUE,
    is_primary               boolean     NOT NULL DEFAULT false,
    verified                 boolean     NOT NULL DEFAULT false,
    verification_token_hash  bytea,
    verification_sent_at     timestamptz,
    verified_at              timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_emails_email_length CHECK (char_length(email::text) BETWEEN 3 AND 254),
    CONSTRAINT user_emails_email_shape  CHECK (email::text LIKE '%@%')
);

-- At most one primary email per user.
CREATE UNIQUE INDEX user_emails_one_primary_per_user
    ON user_emails (user_id) WHERE is_primary;

CREATE INDEX user_emails_user_id_idx ON user_emails (user_id);

-- Now that user_emails exists, attach the FK from users.primary_email_id.
ALTER TABLE users
    ADD CONSTRAINT users_primary_email_fk
        FOREIGN KEY (primary_email_id) REFERENCES user_emails(id)
        ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;

-- +goose Down
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_primary_email_fk;
DROP TABLE IF EXISTS user_emails;
