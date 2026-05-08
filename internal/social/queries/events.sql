-- ─── domain_events ─────────────────────────────────────────────────

-- name: InsertDomainEvent :one
-- Returns the inserted row so callers that want to fire a follow-on
-- (e.g. NOTIFY for the worker pool) have the id without a re-read.
INSERT INTO domain_events (
    actor_user_id, kind, repo_id, source_kind, source_id, public, payload
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING id, actor_user_id, kind, repo_id, source_kind, source_id, public, payload, created_at;

-- name: ListPublicEventsForActor :many
-- Public activity-feed slice for a user's profile. Returns only
-- public rows, recency-sorted. The handler additionally filters by
-- repo visibility against the viewer (a public event row on a repo
-- whose visibility flipped to private must not leak).
SELECT id, actor_user_id, kind, repo_id, source_kind, source_id, public, payload, created_at
FROM domain_events
WHERE actor_user_id = $1
  AND public = true
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListEventsForRepo :many
-- Repo-scoped events, recency-sorted. No visibility filter — the
-- caller has already established read access to the repo.
SELECT id, actor_user_id, kind, repo_id, source_kind, source_id, public, payload, created_at
FROM domain_events
WHERE repo_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;
