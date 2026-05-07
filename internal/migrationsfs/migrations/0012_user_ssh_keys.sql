-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SSH public keys associated with users. Used by sshd's
-- AuthorizedKeysCommand to map an inbound SSH connection to a shithub user.
--
-- fingerprint_sha256 is unique across ALL users, not just per-user — two
-- users registering the same key would produce ambiguous AKC results.
-- Stored without the "SHA256:" prefix so the column carries only the
-- canonical b64-no-padding payload; the prefix is reattached at display time.
--
-- public_key holds the canonical authorized_keys line WITHOUT the comment
-- (we store the title separately). It is the exact blob we re-emit from AKC.

-- +goose Up
CREATE TABLE user_ssh_keys (
    id                 bigserial   PRIMARY KEY,
    user_id            bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title              text        NOT NULL,
    fingerprint_sha256 text        NOT NULL UNIQUE,
    key_type           text        NOT NULL,
    key_bits           integer     NOT NULL DEFAULT 0,
    public_key         text        NOT NULL,
    last_used_at       timestamptz,
    last_used_ip       inet,
    created_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_ssh_keys_title_length CHECK (char_length(title) BETWEEN 1 AND 80),
    CONSTRAINT user_ssh_keys_type_known   CHECK (key_type IN (
        'ssh-ed25519',
        'sk-ssh-ed25519@openssh.com',
        'ecdsa-sha2-nistp256',
        'ecdsa-sha2-nistp384',
        'ecdsa-sha2-nistp521',
        'sk-ecdsa-sha2-nistp256@openssh.com',
        'ssh-rsa'
    ))
);

CREATE INDEX user_ssh_keys_user_id_idx ON user_ssh_keys (user_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS user_ssh_keys;
