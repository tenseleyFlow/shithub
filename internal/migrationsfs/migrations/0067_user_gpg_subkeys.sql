-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Reverse-lookup index for OpenPGP subkey fingerprints. A commit's
-- signature packet carries the SIGNING SUBKEY's fingerprint (or key
-- id), not the primary's; the verification hot path needs a fast
-- (fingerprint → user_id) mapping that doesn't require parsing
-- every user_gpg_keys.armored block.
--
-- The same data is also serialized into user_gpg_keys.subkeys (JSONB)
-- so the REST response can nest subkeys under the primary without
-- a join; both representations are populated atomically when the
-- primary key is inserted.
--
-- Global uniqueness on fingerprint (partial, where revoked_at is null)
-- mirrors the primary table's policy.

-- +goose Up
CREATE TABLE user_gpg_subkeys (
    id                  bigserial   PRIMARY KEY,
    gpg_key_id          bigint      NOT NULL REFERENCES user_gpg_keys(id) ON DELETE CASCADE,
    fingerprint         text        NOT NULL,
    key_id              text        NOT NULL,
    can_sign            boolean     NOT NULL,
    can_encrypt_comms   boolean     NOT NULL,
    can_encrypt_storage boolean     NOT NULL,
    can_certify         boolean     NOT NULL,
    expires_at          timestamptz,
    revoked_at          timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_gpg_subkeys_fingerprint_format CHECK (fingerprint ~ '^[0-9a-f]{40}$'),
    CONSTRAINT user_gpg_subkeys_key_id_format     CHECK (key_id ~ '^[0-9a-f]{16}$')
);

CREATE UNIQUE INDEX user_gpg_subkeys_fingerprint_uniq
    ON user_gpg_subkeys (fingerprint)
    WHERE revoked_at IS NULL;

CREATE INDEX user_gpg_subkeys_key_id_idx     ON user_gpg_subkeys (key_id);
CREATE INDEX user_gpg_subkeys_gpg_key_id_idx ON user_gpg_subkeys (gpg_key_id);

-- +goose Down
DROP INDEX IF EXISTS user_gpg_subkeys_gpg_key_id_idx;
DROP INDEX IF EXISTS user_gpg_subkeys_key_id_idx;
DROP INDEX IF EXISTS user_gpg_subkeys_fingerprint_uniq;
DROP TABLE IF EXISTS user_gpg_subkeys;
