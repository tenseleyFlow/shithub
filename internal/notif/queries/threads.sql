-- ─── notification_threads ──────────────────────────────────────────

-- name: GetNotificationThread :one
SELECT recipient_user_id, thread_kind, thread_id, subscribed, reason, updated_at
FROM notification_threads
WHERE recipient_user_id = $1 AND thread_kind = $2 AND thread_id = $3;

-- name: UpsertNotificationThread :exec
-- Always-write upsert. Used by Subscribe / Unsubscribe / Ignore
-- handlers and by the auto-subscription rules in the fan-out
-- worker.
INSERT INTO notification_threads (recipient_user_id, thread_kind, thread_id, subscribed, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (recipient_user_id, thread_kind, thread_id)
DO UPDATE SET subscribed = EXCLUDED.subscribed,
              reason     = EXCLUDED.reason,
              updated_at = now();

-- name: InsertNotificationThreadIfAbsent :exec
-- Auto-subscription path: only insert if the user has no explicit
-- preference yet. Preserves user choices (e.g. an explicit
-- `subscribed=false` from clicking "Unsubscribe").
INSERT INTO notification_threads (recipient_user_id, thread_kind, thread_id, subscribed, reason)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (recipient_user_id, thread_kind, thread_id) DO NOTHING;

-- name: DeleteNotificationThread :exec
DELETE FROM notification_threads
WHERE recipient_user_id = $1 AND thread_kind = $2 AND thread_id = $3;

-- name: ListSubscribersForThread :many
-- Fan-out helper: returns recipients who explicitly subscribed to a
-- thread. The fan-out worker unions this with the per-repo `watches`
-- result + author/assignee/reviewer rules.
SELECT recipient_user_id, reason
FROM notification_threads
WHERE thread_kind = $1 AND thread_id = $2 AND subscribed = true;
