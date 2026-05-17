-- ─── watches ───────────────────────────────────────────────────────

-- name: GetWatch :one
SELECT user_id, repo_id, level, updated_at
FROM watches WHERE user_id = $1 AND repo_id = $2;

-- name: UpsertWatch :exec
-- Always-write upsert. The AFTER trigger handles the watcher_count
-- delta on transition into / out of `ignore`.
INSERT INTO watches (user_id, repo_id, level)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, repo_id) DO UPDATE
    SET level = EXCLUDED.level,
        updated_at = now();

-- name: InsertWatchIfAbsent :exec
-- Auto-watch flow: only insert if the user doesn't already have a
-- preference. ON CONFLICT DO NOTHING preserves the user's chosen
-- level when the trigger fires repeatedly.
INSERT INTO watches (user_id, repo_id, level)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, repo_id) DO NOTHING;

-- name: DeleteWatch :exec
-- Used when a user unsets their explicit preference (returning to the
-- implicit `participating` default). Trigger drops the watcher_count
-- when the prior level wasn't 'ignore'.
DELETE FROM watches WHERE user_id = $1 AND repo_id = $2;

-- name: ListWatchersForRepo :many
-- Watchers list. `level <> 'ignore'` excludes users who have actively
-- muted the repo. Excludes suspended users from public surfaces.
-- PRO-EXT_SR2-15: select u.plan so the list renders Pro badges.
SELECT w.user_id, w.level, w.updated_at, u.username, u.display_name, u.plan
FROM watches w
JOIN users u ON u.id = w.user_id
WHERE w.repo_id = $1
  AND w.level <> 'ignore'
  AND u.suspended_at IS NULL
ORDER BY w.updated_at DESC
LIMIT $2 OFFSET $3;

-- name: CountWatchersForRepo :one
SELECT COUNT(*) FROM watches w
JOIN users u ON u.id = w.user_id
WHERE w.repo_id = $1
  AND w.level <> 'ignore'
  AND u.suspended_at IS NULL;

-- name: ListRepoWatchersByLevel :many
-- S29 notification-routing consumer: for fan-out, get every watcher
-- of a repo at the requested level (e.g. `level='all'` for new-issue
-- events). This is the cross-package read; expose the user_ids
-- without joining users — fan-out adds the user join itself.
SELECT user_id FROM watches
WHERE repo_id = $1 AND level = $2;
