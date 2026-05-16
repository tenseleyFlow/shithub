-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateDelivery :one
INSERT INTO webhook_relay_deliveries (
    relay_id, destination_url, status, attempt, max_attempts,
    next_attempt_at, payload_bytes, request_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, relay_id, destination_url, status, attempt, max_attempts,
          next_attempt_at, payload_bytes, request_id,
          last_status_code, last_error, delivered_at,
          created_at, updated_at;

-- name: GetDeliveryByID :one
SELECT id, relay_id, destination_url, status, attempt, max_attempts,
       next_attempt_at, payload_bytes, request_id,
       last_status_code, last_error, delivered_at,
       created_at, updated_at
FROM webhook_relay_deliveries
WHERE id = $1;

-- name: ListDeliveriesForRelay :many
SELECT id, relay_id, destination_url, status, attempt, max_attempts,
       next_attempt_at, payload_bytes, request_id,
       last_status_code, last_error, delivered_at,
       created_at, updated_at
FROM webhook_relay_deliveries
WHERE relay_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: MarkDeliverySucceeded :exec
UPDATE webhook_relay_deliveries
SET status = 'succeeded',
    attempt = attempt + 1,
    last_status_code = $2,
    last_error = NULL,
    delivered_at = now(),
    updated_at = now()
WHERE id = $1;

-- name: MarkDeliveryRetry :exec
UPDATE webhook_relay_deliveries
SET status = 'failed_retry',
    attempt = attempt + 1,
    next_attempt_at = $2,
    last_status_code = $3,
    last_error = $4,
    updated_at = now()
WHERE id = $1;

-- name: MarkDeliveryPermanentFailure :exec
UPDATE webhook_relay_deliveries
SET status = 'failed_permanent',
    attempt = attempt + 1,
    last_status_code = $2,
    last_error = $3,
    updated_at = now()
WHERE id = $1;
