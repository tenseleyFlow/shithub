-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Query surface for the webhook package. Naming mirrors the verb the
-- caller uses; visibility / ownership filters are applied by the
-- handler layer (the queries are deliberately narrow on key columns).

-- name: CreateWebhook :one
INSERT INTO webhooks (
    owner_kind, owner_id, url, content_type, events,
    secret_ciphertext, secret_nonce, active, ssl_verification,
    auto_disable_threshold, created_by_user_id
) VALUES (
    sqlc.arg(owner_kind), sqlc.arg(owner_id), sqlc.arg(url),
    sqlc.arg(content_type), sqlc.arg(events),
    sqlc.arg(secret_ciphertext), sqlc.arg(secret_nonce),
    sqlc.arg(active), sqlc.arg(ssl_verification),
    sqlc.arg(auto_disable_threshold),
    sqlc.narg(created_by_user_id)::bigint
)
RETURNING *;

-- name: GetWebhookByID :one
SELECT * FROM webhooks WHERE id = $1;

-- name: ListWebhooksForOwner :many
SELECT * FROM webhooks
WHERE owner_kind = $1 AND owner_id = $2
ORDER BY created_at DESC;

-- name: ListActiveWebhooksForOwner :many
-- Used by fanout to find subscribers for an event.
SELECT * FROM webhooks
WHERE owner_kind = $1 AND owner_id = $2
  AND active = true AND disabled_at IS NULL;

-- name: UpdateWebhook :exec
UPDATE webhooks
   SET url                    = $2,
       content_type           = $3,
       events                 = $4,
       active                 = $5,
       ssl_verification       = $6,
       auto_disable_threshold = $7,
       updated_at             = now()
 WHERE id = $1;

-- name: UpdateWebhookSecret :exec
UPDATE webhooks
   SET secret_ciphertext = $2,
       secret_nonce      = $3,
       updated_at        = now()
 WHERE id = $1;

-- name: DeleteWebhook :exec
DELETE FROM webhooks WHERE id = $1;

-- name: SetWebhookActive :exec
-- Re-enables a previously auto-disabled webhook (UI affordance).
-- Resets the failure counter and clears disabled_at/reason.
UPDATE webhooks
   SET active               = $2,
       disabled_at          = NULL,
       disabled_reason      = NULL,
       consecutive_failures = 0,
       updated_at           = now()
 WHERE id = $1;

-- name: RecordWebhookSuccess :exec
UPDATE webhooks
   SET consecutive_failures = 0,
       last_success_at      = now(),
       updated_at           = now()
 WHERE id = $1;

-- name: RecordWebhookFailure :one
-- Increments the failure counter and reports the new value so the
-- caller can decide whether to auto-disable.
UPDATE webhooks
   SET consecutive_failures = consecutive_failures + 1,
       last_failure_at      = now(),
       updated_at           = now()
 WHERE id = $1
RETURNING consecutive_failures, auto_disable_threshold;

-- name: AutoDisableWebhook :exec
UPDATE webhooks
   SET disabled_at     = now(),
       disabled_reason = $2,
       updated_at      = now()
 WHERE id = $1;

-- name: CreateDelivery :one
INSERT INTO webhook_deliveries (
    webhook_id, event_kind, event_id, payload, request_headers,
    request_body, attempt, max_attempts, next_retry_at, status,
    idempotency_key, redeliver_of
) VALUES (
    sqlc.arg(webhook_id), sqlc.arg(event_kind), sqlc.narg(event_id)::bigint,
    sqlc.arg(payload), sqlc.arg(request_headers), sqlc.arg(request_body),
    sqlc.arg(attempt), sqlc.arg(max_attempts), sqlc.narg(next_retry_at)::timestamptz,
    sqlc.arg(status), sqlc.arg(idempotency_key), sqlc.narg(redeliver_of)::bigint
)
RETURNING *;

-- name: GetDeliveryByID :one
SELECT * FROM webhook_deliveries WHERE id = $1;

-- name: ListDeliveriesForWebhook :many
SELECT id, webhook_id, event_kind, event_id, delivery_uuid, response_status,
       response_truncated, started_at, completed_at, attempt, max_attempts,
       next_retry_at, status, error_summary, redeliver_of
FROM webhook_deliveries
WHERE webhook_id = $1
ORDER BY started_at DESC
LIMIT $2;

-- name: ClaimDueDeliveries :many
-- Hot-path for the deliver worker: picks up to $1 rows that are
-- pending or in retry-ready state, FOR UPDATE SKIP LOCKED so concurrent
-- workers don't double-send. The deliverer marks the row 'pending'
-- with a far-future next_retry_at while it works on it (defense
-- against re-claim during a long HTTP timeout).
SELECT id FROM webhook_deliveries
WHERE status IN ('pending', 'failed_retry')
  AND (next_retry_at IS NULL OR next_retry_at <= now())
ORDER BY next_retry_at NULLS FIRST, id
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: MarkDeliverySucceeded :exec
UPDATE webhook_deliveries
   SET status              = 'succeeded',
       response_status     = $2,
       response_headers    = $3,
       response_body       = $4,
       response_truncated  = $5,
       completed_at        = now(),
       error_summary       = NULL
 WHERE id = $1;

-- name: MarkDeliveryRetry :exec
UPDATE webhook_deliveries
   SET status              = 'failed_retry',
       attempt             = attempt + 1,
       response_status     = sqlc.narg(response_status)::int,
       response_headers    = sqlc.narg(response_headers)::jsonb,
       response_body       = sqlc.narg(response_body)::bytea,
       response_truncated  = sqlc.arg(response_truncated)::bool,
       next_retry_at       = sqlc.arg(next_retry_at)::timestamptz,
       error_summary       = sqlc.arg(error_summary)::text
 WHERE id = sqlc.arg(id)::bigint;

-- name: MarkDeliveryPermanentFailure :exec
UPDATE webhook_deliveries
   SET status              = 'failed_permanent',
       response_status     = sqlc.narg(response_status)::int,
       response_headers    = sqlc.narg(response_headers)::jsonb,
       response_body       = sqlc.narg(response_body)::bytea,
       response_truncated  = sqlc.arg(response_truncated)::bool,
       completed_at        = now(),
       error_summary       = sqlc.arg(error_summary)::text
 WHERE id = sqlc.arg(id)::bigint;

-- name: PurgeOldDeliveries :execrows
-- Cron: drops terminal deliveries older than the retention window.
-- pending/failed_retry rows are left alone so an in-flight retry isn't
-- aborted out from under the worker.
DELETE FROM webhook_deliveries
WHERE status IN ('succeeded', 'failed_permanent')
  AND completed_at < now() - sqlc.arg(retention)::interval;
