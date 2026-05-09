-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Webhook fanout reads the same domain_events / cursor surface that
-- notifications do — but with its own consumer row so progress
-- doesn't collide with notify_fanout.

-- name: GetWebhookCursor :one
SELECT consumer, last_event_id, updated_at
FROM domain_events_processed
WHERE consumer = $1;

-- name: SetWebhookCursor :exec
INSERT INTO domain_events_processed (consumer, last_event_id)
VALUES ($1, $2)
ON CONFLICT (consumer)
DO UPDATE SET last_event_id = EXCLUDED.last_event_id,
              updated_at    = now();

-- name: ListUnprocessedDomainEvents :many
SELECT id, actor_user_id, kind, repo_id, source_kind, source_id,
       public, payload, created_at
FROM domain_events
WHERE id > $1
ORDER BY id
LIMIT $2;

-- name: GetRepoOwnerKindForFanout :one
-- Resolve a repo's owner so the fanout knows which webhooks to fire.
-- Returns owner_kind 'user' or 'org' so the matcher knows what bucket
-- to look in. (We use 'user' as a literal string here; the webhook
-- machinery itself only stores 'repo'/'org' as owners — repo-owned
-- webhooks attach to the repo regardless of who owns the repo.)
SELECT owner_user_id, owner_org_id
FROM repos
WHERE id = $1;
