-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertRunner :one
INSERT INTO workflow_runners (name, labels, capacity, registered_by_user_id)
VALUES ($1, $2, $3, $4)
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          host_name, version, draining_at, drain_reason, revoked_at,
          revoked_reason, registered_by_user_id, created_at, updated_at;

-- name: GetRunnerByID :one
SELECT id, name, labels, capacity, status, last_heartbeat_at,
       host_name, version, draining_at, drain_reason, revoked_at,
       revoked_reason, registered_by_user_id, created_at, updated_at
FROM workflow_runners
WHERE id = $1;

-- name: GetRunnerByName :one
SELECT id, name, labels, capacity, status, last_heartbeat_at,
       host_name, version, draining_at, drain_reason, revoked_at,
       revoked_reason, registered_by_user_id, created_at, updated_at
FROM workflow_runners
WHERE name = $1;

-- name: ListRunners :many
SELECT r.id, r.name, r.labels, r.capacity, r.status, r.last_heartbeat_at,
       r.host_name, r.version, r.draining_at, r.drain_reason, r.revoked_at,
       r.revoked_reason, r.created_at, COUNT(j.id)::integer AS active_job_count
FROM workflow_runners r
LEFT JOIN workflow_jobs j
       ON j.runner_id = r.id
      AND j.status = 'running'
GROUP BY r.id, r.name, r.labels, r.capacity, r.status, r.last_heartbeat_at,
         r.host_name, r.version, r.draining_at, r.drain_reason, r.revoked_at,
         r.revoked_reason, r.created_at
ORDER BY r.name ASC;

-- name: LockRunnerByID :one
SELECT id, name, labels, capacity, status, last_heartbeat_at,
       host_name, version, draining_at, drain_reason, revoked_at,
       revoked_reason, registered_by_user_id, created_at, updated_at
FROM workflow_runners
WHERE id = $1
FOR UPDATE;

-- name: HeartbeatRunner :one
UPDATE workflow_runners
SET labels = $2,
    capacity = $3,
    last_heartbeat_at = now(),
    status = $4,
    host_name = $5,
    version = $6,
    updated_at = now()
WHERE id = $1
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          host_name, version, draining_at, drain_reason, revoked_at,
          revoked_reason, registered_by_user_id, created_at, updated_at;

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
       r.last_heartbeat_at, r.host_name, r.version, r.draining_at,
       r.drain_reason, r.revoked_at, r.revoked_reason, r.created_at
FROM workflow_runners r
JOIN runner_tokens t ON t.runner_id = r.id
WHERE t.token_hash = $1
  AND t.revoked_at IS NULL
  AND r.revoked_at IS NULL
  AND (t.expires_at IS NULL OR t.expires_at > now());

-- name: SetRunnerDraining :one
UPDATE workflow_runners
SET draining_at = COALESCE(draining_at, now()),
    drain_reason = $2,
    updated_at = now()
WHERE id = $1
  AND revoked_at IS NULL
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          host_name, version, draining_at, drain_reason, revoked_at,
          revoked_reason, registered_by_user_id, created_at, updated_at;

-- name: ClearRunnerDraining :one
UPDATE workflow_runners
SET draining_at = NULL,
    drain_reason = '',
    updated_at = now()
WHERE id = $1
  AND revoked_at IS NULL
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          host_name, version, draining_at, drain_reason, revoked_at,
          revoked_reason, registered_by_user_id, created_at, updated_at;

-- name: RevokeRunner :one
UPDATE workflow_runners
SET revoked_at = COALESCE(revoked_at, now()),
    revoked_reason = CASE WHEN revoked_at IS NULL THEN $2 ELSE revoked_reason END,
    draining_at = COALESCE(draining_at, now()),
    drain_reason = CASE
        WHEN draining_at IS NULL THEN $2
        ELSE drain_reason
    END,
    status = 'offline',
    updated_at = now()
WHERE id = $1
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          host_name, version, draining_at, drain_reason, revoked_at,
          revoked_reason, registered_by_user_id, created_at, updated_at;

-- name: RevokeAllTokensForRunner :exec
UPDATE runner_tokens
SET revoked_at = now()
WHERE runner_id = $1 AND revoked_at IS NULL;

-- name: MarkStaleRunnersOffline :many
UPDATE workflow_runners
SET status = 'offline',
    updated_at = now()
WHERE revoked_at IS NULL
  AND status <> 'offline'
  AND last_heartbeat_at IS NOT NULL
  AND last_heartbeat_at < $1
RETURNING id, name, labels, capacity, status, last_heartbeat_at,
          host_name, version, draining_at, drain_reason, revoked_at,
          revoked_reason, registered_by_user_id, created_at, updated_at;
