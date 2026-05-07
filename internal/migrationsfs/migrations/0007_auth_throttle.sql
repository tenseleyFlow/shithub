-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Generic counter table for auth-related rate limits. The (scope, identifier)
-- pair is unique. window_started_at is the start of the current window;
-- callers reset hits to 1 when the window has elapsed.
--
-- Examples of (scope, identifier):
--   ('login',  '1.2.3.4|alice')
--   ('signup', 'ip:1.2.3.4')
--   ('signup', 'email:foo@bar')
--   ('reset',  'email:foo@bar')

-- +goose Up
CREATE TABLE auth_throttle (
    id                 bigserial   PRIMARY KEY,
    scope              text        NOT NULL,
    identifier         text        NOT NULL,
    hits               integer     NOT NULL DEFAULT 0,
    window_started_at  timestamptz NOT NULL DEFAULT now(),

    UNIQUE (scope, identifier)
);

CREATE INDEX auth_throttle_window_started_idx ON auth_throttle (window_started_at);

-- +goose Down
DROP TABLE IF EXISTS auth_throttle;
