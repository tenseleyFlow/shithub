-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29 persisted repository artifact attestations.

-- name: InsertRepoArtifactAttestation :one
INSERT INTO repo_artifact_attestations (
    repo_id,
    subject_name,
    subject_digest,
    predicate_type,
    statement,
    byte_count,
    source_run_id,
    uploaded_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
)
RETURNING *;

-- name: ListRepoArtifactAttestations :many
SELECT *
FROM repo_artifact_attestations
WHERE repo_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2
OFFSET $3;

-- name: GetRepoArtifactAttestationForRepo :one
SELECT *
FROM repo_artifact_attestations
WHERE repo_id = $1
  AND id = $2;
