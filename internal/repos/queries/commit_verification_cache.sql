-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertCommitVerification :exec
-- Idempotent upsert. The verification orchestrator + backfill worker
-- both write through this query; both can safely run concurrently
-- against the same (repo_id, commit_oid) without losing data thanks
-- to the (repo_id, commit_oid) primary key + ON CONFLICT clause.
INSERT INTO commit_verification_cache (
    repo_id, commit_oid, reason, verified,
    signer_user_id, signer_subkey_id, kind,
    signature_armored, payload, verified_at
)
VALUES (
    $1, $2, $3, $4,
    $5, $6, $7,
    $8, $9, now()
)
ON CONFLICT (repo_id, commit_oid) DO UPDATE SET
    reason            = EXCLUDED.reason,
    verified          = EXCLUDED.verified,
    signer_user_id    = EXCLUDED.signer_user_id,
    signer_subkey_id  = EXCLUDED.signer_subkey_id,
    kind              = EXCLUDED.kind,
    signature_armored = EXCLUDED.signature_armored,
    payload           = EXCLUDED.payload,
    verified_at       = now(),
    invalidated_at    = NULL;

-- name: GetCommitVerification :one
-- Single-commit read. Used by the single-commit page renderer and the
-- REST commits/{sha} response. Returns no row when the commit hasn't
-- been verified yet; caller treats that as "compute on demand".
SELECT repo_id, commit_oid, reason, verified,
       signer_user_id, signer_subkey_id, kind,
       signature_armored, payload, verified_at, invalidated_at
FROM commit_verification_cache
WHERE repo_id = $1 AND commit_oid = $2;

-- name: GetCommitVerificationsForOIDs :many
-- Batch read for the commit-list page. Takes an array of OIDs and
-- returns existing rows; missing OIDs are absent from the result and
-- the renderer treats them as "not yet verified".
SELECT repo_id, commit_oid, reason, verified,
       signer_user_id, signer_subkey_id, kind,
       signature_armored, payload, verified_at, invalidated_at
FROM commit_verification_cache
WHERE repo_id = $1 AND commit_oid = ANY($2::text[]);

-- name: InvalidateVerificationsForSubkey :exec
-- Stamps invalidated_at on every cache row whose signer_subkey_id
-- matches. Called from the GPG-key soft-delete path in the same tx as
-- SoftDeleteSubkeysForGPGKey so the cache and the keyring stay in
-- sync. The next read of an invalidated row triggers a re-verify.
UPDATE commit_verification_cache
SET invalidated_at = now()
WHERE signer_subkey_id = $1 AND invalidated_at IS NULL;

-- name: DeleteCommitVerification :exec
-- Used by tests to reset cache state between cases. Not called from
-- production code paths.
DELETE FROM commit_verification_cache
WHERE repo_id = $1 AND commit_oid = $2;
