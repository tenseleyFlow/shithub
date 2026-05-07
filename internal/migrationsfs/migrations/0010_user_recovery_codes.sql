-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Recovery codes. Generated when the user enrolls TOTP, regenerated on
-- demand. Stored as sha256 hashes — the plaintext is shown to the user
-- exactly once at generation time. Single-use via used_at.

-- +goose Up
CREATE TABLE user_recovery_codes (
    id           bigserial   PRIMARY KEY,
    user_id      bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash    bytea       NOT NULL,
    used_at      timestamptz,
    generated_at timestamptz NOT NULL DEFAULT now(),
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX user_recovery_codes_user_id_idx ON user_recovery_codes (user_id);
CREATE UNIQUE INDEX user_recovery_codes_hash_uidx ON user_recovery_codes (code_hash);

-- +goose Down
DROP TABLE IF EXISTS user_recovery_codes;
