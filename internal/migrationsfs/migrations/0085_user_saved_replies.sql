-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-07a: saved replies. A user-scoped library of canned reply
-- snippets the owner can insert into issue / PR comment bodies. Free
-- users get LimitSavedReplies (3) entries; Pro users get unlimited
-- (cap bounded only by DB sanity).
--
-- Slugs are user-scoped: two users can both have a saved reply named
-- "lgtm" without colliding. Names are case-insensitive (citext) so a
-- user typing :lgtm and :LGTM both match the same reply in the
-- compose-time picker that ships in a follow-up.

-- +goose Up
CREATE TABLE user_saved_replies (
    id          bigserial   PRIMARY KEY,
    user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        citext      NOT NULL,
    body        text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_saved_replies_name_shape CHECK (
        char_length(name) BETWEEN 1 AND 80
    ),
    CONSTRAINT user_saved_replies_body_shape CHECK (
        char_length(body) BETWEEN 1 AND 8000
    ),
    CONSTRAINT user_saved_replies_user_name_unique UNIQUE (user_id, name)
);

CREATE INDEX user_saved_replies_user_id_idx
    ON user_saved_replies (user_id, name);

-- +goose Down
DROP TABLE IF EXISTS user_saved_replies;
