-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: AppendStepLogChunk :one
INSERT INTO workflow_step_log_chunks (step_id, seq, chunk)
VALUES ($1, $2, $3)
ON CONFLICT (step_id, seq) DO NOTHING
RETURNING id, step_id, seq, created_at;

-- name: ListStepLogChunks :many
SELECT id, step_id, seq, chunk, created_at
FROM workflow_step_log_chunks
WHERE step_id = $1 AND seq > $2
ORDER BY seq ASC
LIMIT $3;

-- name: DeleteStepLogChunks :exec
DELETE FROM workflow_step_log_chunks WHERE step_id = $1;
