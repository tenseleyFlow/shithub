-- ─── domain_events_processed ──────────────────────────────────────

-- name: GetEventCursor :one
SELECT consumer, last_event_id, updated_at
FROM domain_events_processed
WHERE consumer = $1;

-- name: SetEventCursor :exec
-- Always-write upsert so the worker doesn't have to special-case
-- the missing-row branch (the migration seeds 'notify_fanout' at
-- 0; future consumers like 'webhook_deliver' do the same on first
-- run via this same call).
INSERT INTO domain_events_processed (consumer, last_event_id)
VALUES ($1, $2)
ON CONFLICT (consumer)
DO UPDATE SET last_event_id = EXCLUDED.last_event_id,
              updated_at    = now();

-- name: ListUnprocessedDomainEvents :many
-- The fan-out worker's read cursor. Bounded so a single tick
-- doesn't try to drain a million-row backlog.
SELECT id, actor_user_id, kind, repo_id, source_kind, source_id,
       public, payload, created_at
FROM domain_events
WHERE id > $1
ORDER BY id
LIMIT $2;
