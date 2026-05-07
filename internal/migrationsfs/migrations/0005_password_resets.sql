-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Password reset tokens. The token itself is high-entropy random; only its
-- sha256 hash is stored. used_at marks single-use consumption (replay
-- protection). expires_at is set to created_at + 1 hour at insert time.

-- +goose Up
CREATE TABLE password_resets (
    id          bigserial   PRIMARY KEY,
    user_id     bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX password_resets_user_id_idx ON password_resets (user_id);
CREATE INDEX password_resets_expires_at_idx ON password_resets (expires_at);

-- +goose Down
DROP TABLE IF EXISTS password_resets;
