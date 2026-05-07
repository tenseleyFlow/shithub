-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Core users table. username is citext for case-insensitive uniqueness;
-- display casing is preserved for UI rendering. password_hash stores a
-- PHC-encoded argon2id string; password_algo identifies the encoding so
-- we can roll forward later without DB-side migration.
--
-- primary_email_id is a forward-reference to user_emails.id (FK added
-- after that table exists in 0004 to avoid circular DDL).

-- +goose Up
CREATE TABLE users (
    id                  bigserial   PRIMARY KEY,
    username            citext      NOT NULL UNIQUE,
    display_name        text        NOT NULL DEFAULT '',
    primary_email_id    bigint,
    password_hash       text        NOT NULL,
    password_algo       text        NOT NULL DEFAULT 'argon2id-v1',
    password_updated_at timestamptz NOT NULL DEFAULT now(),
    email_verified      boolean     NOT NULL DEFAULT false,
    last_login_at       timestamptz,
    suspended_at        timestamptz,
    suspended_reason    text,
    deleted_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_username_length CHECK (char_length(username::text) BETWEEN 1 AND 39),
    CONSTRAINT users_password_algo CHECK (password_algo IN ('argon2id-v1'))
);

CREATE INDEX users_deleted_at_idx ON users (deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX users_suspended_at_idx ON users (suspended_at) WHERE suspended_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS users;
