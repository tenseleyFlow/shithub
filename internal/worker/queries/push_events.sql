-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- push_events records what landed; the post-receive hook writes; the
-- push:process job consumes.

-- name: InsertPushEvent :one
INSERT INTO push_events (
    repo_id, pusher_user_id, before_sha, after_sha, ref, protocol, request_id
) VALUES (
    $1, sqlc.narg(pusher_user_id)::bigint, $2, $3, $4, $5, COALESCE(sqlc.narg(request_id)::text, '')
)
RETURNING *;

-- name: GetPushEvent :one
SELECT * FROM push_events WHERE id = $1;

-- name: MarkPushEventProcessed :exec
UPDATE push_events
SET processed_at = now()
WHERE id = $1;
