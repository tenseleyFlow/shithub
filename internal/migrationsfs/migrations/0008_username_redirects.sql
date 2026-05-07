-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Username change history. When a user changes their username (S10), the
-- old name is recorded here so:
--   1. URLs to /<old-username>/* can 301 to the new username.
--   2. The old name is reserved for a 30-day cooldown — the redirect
--      record itself acts as the reservation; the signup flow checks
--      this table in addition to users.username when validating new names.

-- +goose Up
CREATE TABLE username_redirects (
    old_username text        PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    changed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX username_redirects_user_id_idx ON username_redirects (user_id);
CREATE INDEX username_redirects_changed_at_idx ON username_redirects (changed_at);

-- +goose Down
DROP TABLE IF EXISTS username_redirects;
