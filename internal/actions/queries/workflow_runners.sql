-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertRunner :one
INSERT INTO workflow_runners (name, labels, capacity, registered_by_user_id)
VALUES ($1, $2, $3, $4)
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          registered_by_user_id, created_at, updated_at;

-- name: GetRunnerByID :one
SELECT id, name, labels, capacity, status, last_heartbeat_at,
       registered_by_user_id, created_at, updated_at
FROM workflow_runners
WHERE id = $1;

-- name: GetRunnerByName :one
SELECT id, name, labels, capacity, status, last_heartbeat_at,
       registered_by_user_id, created_at, updated_at
FROM workflow_runners
WHERE name = $1;

-- name: ListRunners :many
SELECT id, name, labels, capacity, status, last_heartbeat_at, created_at
FROM workflow_runners
ORDER BY name ASC;

-- name: TouchRunnerHeartbeat :exec
UPDATE workflow_runners
SET last_heartbeat_at = now(),
    status = $2,
    updated_at = now()
WHERE id = $1;

-- name: InsertRunnerToken :one
INSERT INTO runner_tokens (runner_id, token_hash, expires_at)
VALUES ($1, $2, $3)
RETURNING id, runner_id, token_hash, expires_at, revoked_at, created_at;

-- name: GetRunnerByTokenHash :one
SELECT r.id, r.name, r.labels, r.capacity, r.status,
       r.last_heartbeat_at, r.created_at
FROM workflow_runners r
JOIN runner_tokens t ON t.runner_id = r.id
WHERE t.token_hash = $1
  AND t.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now());

-- name: RevokeAllTokensForRunner :exec
UPDATE runner_tokens
SET revoked_at = now()
WHERE runner_id = $1 AND revoked_at IS NULL;
