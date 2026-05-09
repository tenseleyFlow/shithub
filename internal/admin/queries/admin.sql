-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Admin-only query surface for /admin/*. Non-admin handlers should
-- use the per-domain packages (usersdb, reposdb, …) directly. This
-- file collects the cross-cutting reads + writes the admin UI needs:
--   * dashboard counts
--   * user/repo listings with admin-only filters (suspended, deleted)
--   * site-admin flag toggle
--   * transactional_email_log read/write
--
-- Queries that already exist in their domain package (e.g.
-- users.SuspendUser) are reused directly — no duplication here.

-- ─── dashboard counts ────────────────────────────────────────────

-- name: CountActiveUsers :one
SELECT COUNT(*) FROM users WHERE deleted_at IS NULL;

-- name: CountSuspendedUsers :one
SELECT COUNT(*) FROM users WHERE suspended_at IS NOT NULL AND deleted_at IS NULL;

-- name: CountSiteAdmins :one
SELECT COUNT(*) FROM users WHERE is_site_admin = true AND deleted_at IS NULL;

-- name: CountActiveRepos :one
SELECT COUNT(*) FROM repos WHERE deleted_at IS NULL;

-- name: CountActiveOrgs :one
SELECT COUNT(*) FROM orgs WHERE deleted_at IS NULL;

-- name: CountJobsByStatus :many
-- Status is derived from the timestamp columns since the jobs table
-- doesn't carry an enum: completed > failed > running > queued.
SELECT
    CASE
        WHEN completed_at IS NOT NULL THEN 'completed'
        WHEN failed_at    IS NOT NULL THEN 'failed'
        WHEN locked_at    IS NOT NULL THEN 'running'
        ELSE 'queued'
    END AS status,
    COUNT(*)::bigint AS n
FROM jobs
GROUP BY 1;

-- ─── users list ──────────────────────────────────────────────────

-- name: ListUsersForAdmin :many
-- Filters: optional username prefix, optional suspended/deleted-only.
-- All filters use sqlc.narg so the empty case is "no filter".
SELECT id, username, display_name, primary_email_id,
       suspended_at, deleted_at, last_login_at, is_site_admin,
       created_at
FROM users
WHERE
    (sqlc.narg(username_prefix)::text IS NULL OR username::text ILIKE sqlc.narg(username_prefix)::text || '%')
    AND (sqlc.narg(suspended_only)::bool IS NOT TRUE OR suspended_at IS NOT NULL)
    AND (sqlc.narg(deleted_only)::bool IS NOT TRUE OR deleted_at IS NOT NULL)
ORDER BY id DESC
LIMIT $1 OFFSET $2;

-- name: SetUserSiteAdmin :exec
UPDATE users
   SET is_site_admin = $2,
       updated_at    = now()
 WHERE id = $1;

-- ─── repos list ──────────────────────────────────────────────────

-- name: ListReposForAdmin :many
SELECT r.id, r.owner_user_id, r.owner_org_id, r.name, r.visibility,
       r.is_archived, r.deleted_at, r.disk_used_bytes, r.created_at,
       u.username AS owner_user_username,
       o.slug     AS owner_org_slug
FROM repos r
LEFT JOIN users u ON u.id = r.owner_user_id
LEFT JOIN orgs  o ON o.id = r.owner_org_id
WHERE
    (sqlc.narg(name_prefix)::text IS NULL OR r.name ILIKE sqlc.narg(name_prefix)::text || '%')
    AND (sqlc.narg(deleted_only)::bool IS NOT TRUE OR r.deleted_at IS NOT NULL)
    AND (sqlc.narg(archived_only)::bool IS NOT TRUE OR r.is_archived = true)
    AND (sqlc.narg(visibility_filter)::repo_visibility IS NULL OR r.visibility = sqlc.narg(visibility_filter)::repo_visibility)
ORDER BY r.id DESC
LIMIT $1 OFFSET $2;

-- ─── jobs queue inspector ────────────────────────────────────────

-- name: ListJobsForAdmin :many
-- The admin can filter by kind + status (computed from the timestamp
-- columns). Status filter values: queued | running | failed | completed.
SELECT id, kind, payload, run_at, attempts, max_attempts,
       last_error, locked_by, locked_at, completed_at, failed_at, created_at,
       CASE
           WHEN completed_at IS NOT NULL THEN 'completed'
           WHEN failed_at    IS NOT NULL THEN 'failed'
           WHEN locked_at    IS NOT NULL THEN 'running'
           ELSE 'queued'
       END AS status
FROM jobs
WHERE
    (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
    AND (
        sqlc.narg(status_filter)::text IS NULL
        OR (sqlc.narg(status_filter)::text = 'queued'    AND completed_at IS NULL AND failed_at IS NULL AND locked_at IS NULL)
        OR (sqlc.narg(status_filter)::text = 'running'   AND completed_at IS NULL AND failed_at IS NULL AND locked_at IS NOT NULL)
        OR (sqlc.narg(status_filter)::text = 'failed'    AND failed_at IS NOT NULL)
        OR (sqlc.narg(status_filter)::text = 'completed' AND completed_at IS NOT NULL)
    )
ORDER BY id DESC
LIMIT $1 OFFSET $2;

-- name: GetJobForAdmin :one
SELECT id, kind, payload, run_at, attempts, max_attempts,
       last_error, locked_by, locked_at, completed_at, failed_at, created_at
FROM jobs
WHERE id = $1;

-- name: AdminRetryJob :exec
-- Re-arm a failed/dead job for immediate retry: clear failure state,
-- reset attempts, and set run_at to now() so the pool picks it up
-- on the next tick.
UPDATE jobs
   SET run_at       = now(),
       attempts     = 0,
       last_error   = NULL,
       failed_at    = NULL,
       completed_at = NULL,
       locked_by    = NULL,
       locked_at    = NULL
 WHERE id = $1;

-- name: AdminDiscardJob :exec
-- Mark a job dead without running it. Caller has already confirmed.
UPDATE jobs
   SET failed_at  = now(),
       last_error = 'discarded by site admin'
 WHERE id = $1;

-- ─── audit log viewer ────────────────────────────────────────────

-- name: ListAuditForAdmin :many
-- Filters: actor, action prefix, target type+id, time range. Keyset
-- pagination on (created_at, id) keeps deep pages cheap.
SELECT id, actor_id, action, target_type, target_id, meta, created_at
FROM auth_audit_log
WHERE
    (sqlc.narg(actor_id)::bigint  IS NULL OR actor_id = sqlc.narg(actor_id)::bigint)
    AND (sqlc.narg(action_prefix)::text IS NULL OR action ILIKE sqlc.narg(action_prefix)::text || '%')
    AND (sqlc.narg(target_type)::text IS NULL OR target_type = sqlc.narg(target_type)::text)
    AND (sqlc.narg(target_id)::bigint IS NULL OR target_id = sqlc.narg(target_id)::bigint)
    AND (sqlc.narg(since)::timestamptz IS NULL OR created_at >= sqlc.narg(since)::timestamptz)
    AND (sqlc.narg(until)::timestamptz IS NULL OR created_at <  sqlc.narg(until)::timestamptz)
ORDER BY created_at DESC, id DESC
LIMIT $1 OFFSET $2;

-- ─── transactional email log ─────────────────────────────────────

-- name: InsertTransactionalEmail :one
INSERT INTO transactional_email_log (
    recipient_user_id, recipient_email, kind, subject, provider_id,
    status, error_summary
) VALUES (
    sqlc.narg(recipient_user_id)::bigint,
    sqlc.arg(recipient_email),
    sqlc.arg(kind),
    sqlc.arg(subject),
    sqlc.arg(provider_id),
    sqlc.arg(status),
    sqlc.narg(error_summary)::text
)
RETURNING id;

-- name: ListRecentTransactionalEmails :many
-- Admin email-queue surface. Most recent first; status filter optional.
SELECT id, recipient_user_id, recipient_email, kind, subject,
       provider_id, status, error_summary, sent_at, delivered_at
FROM transactional_email_log
WHERE (sqlc.narg(status)::transactional_email_status IS NULL
       OR status = sqlc.narg(status)::transactional_email_status)
ORDER BY sent_at DESC
LIMIT $1;

-- name: MarkTransactionalEmailDelivered :exec
UPDATE transactional_email_log
   SET status        = 'sent',
       delivered_at  = now(),
       error_summary = NULL
 WHERE id = $1;

-- name: MarkTransactionalEmailFailed :exec
UPDATE transactional_email_log
   SET status        = $2,
       error_summary = $3
 WHERE id = $1;
