-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-07b: scheduled issues. The settings page reads
-- (Pending, Recent) lists; the worker handler reads a single row by
-- id at job time.

-- name: InsertScheduledIssue :one
INSERT INTO user_scheduled_issues (user_id, repo_id, title, body, schedule_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetScheduledIssue :one
-- Scoped by user_id for handler-side cancel/view; the worker calls
-- GetScheduledIssueByID below (no user_id filter, since the worker is
-- system-level).
SELECT *
FROM user_scheduled_issues
WHERE id = $1 AND user_id = $2;

-- name: GetScheduledIssueByID :one
-- Worker-only: no user_id filter. The worker uses status to decide
-- whether to short-circuit (cancelled / already created).
SELECT *
FROM user_scheduled_issues
WHERE id = $1;

-- name: ListScheduledIssuesForUser :many
-- Settings page: pending first (sorted by schedule_at), then recent
-- non-pending. Limit prevents an unbounded scan in pathological data.
SELECT *
FROM user_scheduled_issues
WHERE user_id = $1
ORDER BY
    CASE WHEN status = 'pending' THEN 0 ELSE 1 END,
    schedule_at DESC
LIMIT 200;

-- name: CountPendingScheduledIssuesForUser :one
SELECT count(*) FROM user_scheduled_issues
WHERE user_id = $1 AND status = 'pending';

-- name: CancelScheduledIssue :exec
-- Idempotent: only flips status when still pending. Caller scopes by
-- user_id so a misaddressed id is a no-op.
UPDATE user_scheduled_issues
SET status = 'cancelled', updated_at = now()
WHERE id = $1 AND user_id = $2 AND status = 'pending';

-- name: MarkScheduledIssueCreated :exec
UPDATE user_scheduled_issues
SET status = 'created', created_issue_id = $2, updated_at = now()
WHERE id = $1;

-- name: MarkScheduledIssueFailed :exec
UPDATE user_scheduled_issues
SET status = 'failed', failure_reason = $2, updated_at = now()
WHERE id = $1;
