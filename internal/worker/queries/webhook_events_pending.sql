-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- The S33 webhook deliverer drains this. S14 only inserts; the
-- accumulator is intentionally separate from the generic jobs table so
-- S33 controls its own retry / batching / fan-out shape.

-- name: InsertWebhookEventPending :one
INSERT INTO webhook_events_pending (repo_id, event_kind, payload)
VALUES ($1, $2, $3)
RETURNING *;
