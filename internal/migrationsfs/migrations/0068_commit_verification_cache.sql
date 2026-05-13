-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Cache for commit and annotated-tag signature verification results.
-- Read by the rendering paths (commit list / single commit / tag list),
-- written by the verification orchestrator and the backfill worker.
--
-- Keyed on (repo_id, commit_oid) so each (repo, object) pair has at
-- most one row. The `kind` discriminator separates commit-object
-- verifications from tag-object verifications; both reuse the same
-- schema because their `reason` enum + signer fields are identical.
--
-- reason values mirror GitHub's documented enum on the commit
-- verification object: valid, unsigned, unknown_key, bad_email,
-- unverified_email, malformed_signature, invalid, expired_key,
-- not_signing_key. `verified` is the convenience boolean (reason ==
-- 'valid').
--
-- signer_user_id and signer_subkey_id are nullable: set whenever the
-- signature matched a stored key cryptographically (even if the
-- email cross-check or expiry then disqualified it). Both are
-- ON DELETE SET NULL so revoking a key or deleting a user doesn't
-- cascade-delete historical verification rows — the row remains as
-- "this was once verified by a key we no longer have", and the
-- rendering path treats it as unverified.
--
-- invalidated_at is the cache-staleness flag: stamped by the GPG-key
-- soft-delete path on every dependent row in the same tx. The next
-- read triggers a re-verify; the backfill worker catches up in the
-- background.

-- +goose Up
CREATE TABLE commit_verification_cache (
    repo_id           bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    commit_oid        text        NOT NULL,
    reason            text        NOT NULL,
    verified          boolean     NOT NULL,
    signer_user_id    bigint      REFERENCES users(id) ON DELETE SET NULL,
    signer_subkey_id  bigint      REFERENCES user_gpg_subkeys(id) ON DELETE SET NULL,
    kind              text        NOT NULL,
    signature_armored text,
    payload           bytea,
    verified_at       timestamptz NOT NULL DEFAULT now(),
    invalidated_at    timestamptz,

    PRIMARY KEY (repo_id, commit_oid),

    CONSTRAINT commit_verification_cache_kind_known CHECK (kind IN ('commit', 'tag')),
    CONSTRAINT commit_verification_cache_reason_known CHECK (reason IN (
        'valid',
        'unsigned',
        'unknown_key',
        'bad_email',
        'unverified_email',
        'malformed_signature',
        'invalid',
        'expired_key',
        'not_signing_key'
    )),
    CONSTRAINT commit_verification_cache_verified_implies_valid CHECK (
        (verified = false) OR (reason = 'valid')
    ),
    CONSTRAINT commit_verification_cache_oid_format CHECK (commit_oid ~ '^[0-9a-f]{40}$')
);

CREATE INDEX commit_verification_cache_signer_user_idx
    ON commit_verification_cache (signer_user_id)
    WHERE signer_user_id IS NOT NULL;

CREATE INDEX commit_verification_cache_signer_subkey_idx
    ON commit_verification_cache (signer_subkey_id)
    WHERE signer_subkey_id IS NOT NULL;

CREATE INDEX commit_verification_cache_invalidated_idx
    ON commit_verification_cache (invalidated_at)
    WHERE invalidated_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS commit_verification_cache_invalidated_idx;
DROP INDEX IF EXISTS commit_verification_cache_signer_subkey_idx;
DROP INDEX IF EXISTS commit_verification_cache_signer_user_idx;
DROP TABLE IF EXISTS commit_verification_cache;
