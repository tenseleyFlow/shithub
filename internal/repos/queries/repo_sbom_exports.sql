-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP29 persisted repository SBOM exports.

-- name: UpsertRepoSBOMExport :one
INSERT INTO repo_sbom_exports (
    repo_id,
    format,
    source_head_sha,
    dependency_snapshot_generated_at,
    document,
    byte_count,
    generated_by,
    generated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, now()
)
ON CONFLICT (repo_id, format) DO UPDATE
SET source_head_sha = EXCLUDED.source_head_sha,
    dependency_snapshot_generated_at = EXCLUDED.dependency_snapshot_generated_at,
    document = EXCLUDED.document,
    byte_count = EXCLUDED.byte_count,
    generated_by = EXCLUDED.generated_by,
    generated_at = now()
RETURNING *;

-- name: GetRepoSBOMExport :one
SELECT *
FROM repo_sbom_exports
WHERE repo_id = $1
  AND format = $2;
