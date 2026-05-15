-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-08a: saved search queries. Pro users can name and persist
-- a code-search query string, an optional repo/owner scope filter, and
-- a flag for whether the query body is plain text vs regex (regex is
-- itself a Pro-gated feature shipped in PRO-EXT01-08b). Free users
-- reach the settings page but the form is pro-lock'd.
--
-- Names are (user_id, name) UNIQUE — case-insensitive via citext so
-- :recent and :Recent collide. Same shape as user_saved_replies.

-- +goose Up
CREATE TYPE code_search_query_kind AS ENUM ('plain', 'regex');

CREATE TABLE user_code_search_saved_queries (
    id          bigserial   PRIMARY KEY,
    user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        citext      NOT NULL,
    query_text  text        NOT NULL,
    kind        code_search_query_kind NOT NULL DEFAULT 'plain',
    -- scope_filter is an optional `owner/repo` or `owner` string that
    -- the search handler applies as a repo filter at query time. Stored
    -- as opaque text rather than a normalized FK pair so a saved query
    -- survives an owner/repo rename (the user re-edits if it stops
    -- matching).
    scope_filter text       NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_code_search_saved_queries_name_shape CHECK (
        char_length(name) BETWEEN 1 AND 80
    ),
    CONSTRAINT user_code_search_saved_queries_query_shape CHECK (
        char_length(query_text) BETWEEN 1 AND 1000
    ),
    CONSTRAINT user_code_search_saved_queries_scope_shape CHECK (
        char_length(scope_filter) BETWEEN 0 AND 200
    ),
    CONSTRAINT user_code_search_saved_queries_user_name_unique UNIQUE (user_id, name)
);

CREATE INDEX user_code_search_saved_queries_user_id_idx
    ON user_code_search_saved_queries (user_id, updated_at DESC);

-- +goose Down
DROP TABLE IF EXISTS user_code_search_saved_queries;
DROP TYPE IF EXISTS code_search_query_kind;
