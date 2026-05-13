-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- OpenPGP public keys associated with users. Used to verify the
-- signature on a commit or annotated tag and render the "Verified"
-- badge. Companion table user_gpg_subkeys (0067) carries the per-
-- subkey reverse-lookup index that the verification hot path joins
-- against — a commit signature carries the *subkey's* fingerprint,
-- not the primary's.
--
-- fingerprint is unique across ALL users (partial index, where
-- revoked_at is null) — two users registering the same key would
-- produce ambiguous verification lookups. Soft-delete via revoked_at
-- preserves audit history and lets re-upload of the same fingerprint
-- after revoke succeed.
--
-- armored holds the ASCII-armored block exactly as uploaded so we
-- can round-trip it back over REST and email; the parsed capability
-- flags + uids + subkey metadata are decoded once at insert time
-- and stored alongside so the REST response doesn't re-parse on read.
--
-- can_encrypt_comms vs can_encrypt_storage split per RFC 4880
-- §5.2.3.21 to match GitHub's /user/gpg_keys response shape exactly.
-- can_authenticate is stored but not surfaced over REST in S51
-- (GitHub doesn't surface it either; the column lets S52/S53 expose
-- it later without a schema change).

-- +goose Up
CREATE TABLE user_gpg_keys (
    id                  bigserial   PRIMARY KEY,
    user_id             bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                text        NOT NULL DEFAULT '',
    fingerprint         text        NOT NULL,
    key_id              text        NOT NULL,
    armored             text        NOT NULL,
    can_sign            boolean     NOT NULL,
    can_encrypt_comms   boolean     NOT NULL,
    can_encrypt_storage boolean     NOT NULL,
    can_certify         boolean     NOT NULL,
    can_authenticate    boolean     NOT NULL,
    uids                text[]      NOT NULL DEFAULT '{}',
    subkeys             jsonb       NOT NULL DEFAULT '[]'::jsonb,
    primary_algo        text        NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_used_at        timestamptz,
    revoked_at          timestamptz,
    expires_at          timestamptz,

    CONSTRAINT user_gpg_keys_name_length        CHECK (char_length(name) <= 80),
    CONSTRAINT user_gpg_keys_fingerprint_format CHECK (fingerprint ~ '^[0-9a-f]{40}$'),
    CONSTRAINT user_gpg_keys_key_id_format     CHECK (key_id ~ '^[0-9a-f]{16}$')
);

CREATE UNIQUE INDEX user_gpg_keys_fingerprint_uniq
    ON user_gpg_keys (fingerprint)
    WHERE revoked_at IS NULL;

CREATE INDEX user_gpg_keys_user_id_idx ON user_gpg_keys (user_id, created_at DESC);
CREATE INDEX user_gpg_keys_key_id_idx  ON user_gpg_keys (key_id);

-- +goose Down
DROP INDEX IF EXISTS user_gpg_keys_key_id_idx;
DROP INDEX IF EXISTS user_gpg_keys_user_id_idx;
DROP INDEX IF EXISTS user_gpg_keys_fingerprint_uniq;
DROP TABLE IF EXISTS user_gpg_keys;
