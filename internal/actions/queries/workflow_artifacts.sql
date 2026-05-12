-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertArtifact :one
INSERT INTO workflow_artifacts (run_id, name, object_key, byte_count, expires_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, run_id, name, object_key, byte_count, expires_at, created_at;

-- name: ListArtifactsForRun :many
SELECT id, name, object_key, byte_count, expires_at, created_at
FROM workflow_artifacts
WHERE run_id = $1
ORDER BY name ASC;

-- name: GetArtifactByID :one
SELECT id, run_id, name, object_key, byte_count, expires_at, created_at
FROM workflow_artifacts
WHERE id = $1;

-- name: ListExpiredWorkflowArtifactsForCleanup :many
SELECT id, object_key
FROM workflow_artifacts
WHERE expires_at < $1
  AND object_key LIKE 'actions/runs/%'
ORDER BY expires_at ASC, id ASC
LIMIT $2;

-- name: DeleteWorkflowArtifactsByIDs :execrows
DELETE FROM workflow_artifacts
WHERE id = ANY($1::bigint[]);
