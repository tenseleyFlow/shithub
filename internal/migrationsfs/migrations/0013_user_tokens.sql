-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Personal access tokens. Authenticate API calls and git-over-HTTPS
-- basic-auth (username = shithub username; password = the raw token).
--
-- token_hash is sha256 of the raw token (32 bytes b62) — token inputs
-- are uniform-random, so unsalted sha256 has no rainbow-table surface.
-- token_prefix is the first ~12 chars of the raw token (incl. the
-- shithub_pat_ marker), stored ONLY for display in the listing page so
-- users can identify which token is which.
--
-- scopes is a Postgres text[] with values from a fixed enum at the app
-- layer; we don't enforce that at the DB to keep migrations cheap.
--
-- expires_at NULL means non-expiring (UI nudges users toward 90 days).
-- revoked_at flips the row to denied without losing the audit trail.

-- +goose Up
CREATE TABLE user_tokens (
    id            bigserial   PRIMARY KEY,
    user_id       bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          text        NOT NULL,
    token_hash    bytea       NOT NULL UNIQUE,
    token_prefix  text        NOT NULL,
    scopes        text[]      NOT NULL DEFAULT ARRAY[]::text[],
    expires_at    timestamptz,
    last_used_at  timestamptz,
    last_used_ip  inet,
    revoked_at    timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_tokens_name_length CHECK (char_length(name) BETWEEN 1 AND 80),
    CONSTRAINT user_tokens_hash_size   CHECK (octet_length(token_hash) = 32)
);

CREATE INDEX user_tokens_user_id_idx    ON user_tokens (user_id, created_at DESC);
CREATE INDEX user_tokens_revoked_at_idx ON user_tokens (revoked_at) WHERE revoked_at IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS user_tokens;
