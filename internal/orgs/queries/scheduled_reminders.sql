-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── org_scheduled_reminders ─────────────────────────────────────

-- name: CreateOrgScheduledReminder :one
INSERT INTO org_scheduled_reminders (
    org_id, name, target, repo_id, team_id, cron_expr, timezone,
    next_run_at, condition_review_requested,
    condition_team_review_requested, min_age_minutes, created_by_user_id
) VALUES (
    $1, $2, $3, sqlc.narg(repo_id)::bigint, sqlc.narg(team_id)::bigint,
    $4, $5, $6, $7, $8, $9, sqlc.narg(created_by_user_id)::bigint
)
RETURNING *;

-- name: GetOrgScheduledReminder :one
SELECT * FROM org_scheduled_reminders WHERE id = $1;

-- name: ListOrgScheduledReminders :many
SELECT sr.*,
       coalesce(r.name, '') AS repo_name,
       coalesce(t.slug, '') AS team_slug,
       coalesce(t.display_name, '') AS team_display_name
FROM org_scheduled_reminders sr
LEFT JOIN repos r ON r.id = sr.repo_id
LEFT JOIN teams t ON t.id = sr.team_id
WHERE sr.org_id = $1
ORDER BY sr.created_at DESC, sr.id DESC;

-- name: UpdateOrgScheduledReminder :one
UPDATE org_scheduled_reminders
SET name = $2,
    target = $3,
    repo_id = sqlc.narg(repo_id)::bigint,
    team_id = sqlc.narg(team_id)::bigint,
    cron_expr = $4,
    timezone = $5,
    next_run_at = $6,
    condition_review_requested = $7,
    condition_team_review_requested = $8,
    min_age_minutes = $9,
    paused_at = sqlc.narg(paused_at)::timestamptz,
    last_run_status = 'pending',
    last_run_error = NULL
WHERE id = $1 AND org_id = $10
RETURNING *;

-- name: PauseOrgScheduledReminder :exec
UPDATE org_scheduled_reminders
SET paused_at = coalesce(paused_at, now())
WHERE id = $1 AND org_id = $2;

-- name: ResumeOrgScheduledReminder :one
UPDATE org_scheduled_reminders
SET paused_at = NULL,
    next_run_at = $3,
    last_run_status = 'pending',
    last_run_error = NULL
WHERE id = $1 AND org_id = $2
RETURNING *;

-- name: DeleteOrgScheduledReminder :exec
DELETE FROM org_scheduled_reminders WHERE id = $1 AND org_id = $2;

-- name: CountPrivateReposForScheduledReminderTarget :one
SELECT count(*)::int
FROM repos r
WHERE r.owner_org_id = $1
  AND r.deleted_at IS NULL
  AND r.visibility = 'private'
  AND (
    sqlc.arg(target)::org_scheduled_reminder_target = 'all_repositories'
    OR (sqlc.arg(target)::org_scheduled_reminder_target = 'repository' AND r.id = sqlc.narg(repo_id)::bigint)
    OR (
      sqlc.arg(target)::org_scheduled_reminder_target = 'team'
      AND EXISTS (
        SELECT 1
        FROM team_repo_access tra
        WHERE tra.repo_id = r.id
          AND tra.team_id = sqlc.narg(team_id)::bigint
      )
    )
  );

-- name: ClaimDueOrgScheduledReminders :many
SELECT *
FROM org_scheduled_reminders
WHERE paused_at IS NULL
  AND next_run_at <= now()
ORDER BY next_run_at ASC, id ASC
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: AdvanceOrgScheduledReminder :exec
UPDATE org_scheduled_reminders
SET next_run_at = $2,
    last_run_at = now(),
    last_run_status = $3,
    last_run_error = $4
WHERE id = $1;

-- name: ClaimOrgScheduledReminderDelivery :one
INSERT INTO org_scheduled_reminder_deliveries (
    schedule_id, run_key, pr_issue_id, recipient_user_id
) VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING
RETURNING schedule_id, run_key, pr_issue_id, recipient_user_id, notification_id, delivered_at;

-- name: MarkOrgScheduledReminderDeliverySent :exec
UPDATE org_scheduled_reminder_deliveries
SET notification_id = $5,
    delivered_at = now()
WHERE schedule_id = $1
  AND run_key = $2
  AND pr_issue_id = $3
  AND recipient_user_id = $4;

-- name: ListDirectReviewReminderCandidates :many
SELECT rr.id AS review_request_id,
       i.id AS pr_issue_id,
       i.number AS pr_number,
       i.title AS pr_title,
       i.author_user_id,
       r.id AS repo_id,
       r.owner_user_id,
       r.owner_org_id,
       r.visibility::text AS repo_visibility,
       r.is_archived,
       r.deleted_at,
       r.is_paused,
       u.id AS recipient_user_id,
       u.username AS recipient_username,
       (u.suspended_at IS NOT NULL)::bool AS recipient_suspended,
       u.is_site_admin AS recipient_site_admin
FROM org_scheduled_reminders sr
JOIN repos r ON r.owner_org_id = sr.org_id
JOIN issues i ON i.repo_id = r.id AND i.kind = 'pr' AND i.state = 'open'
JOIN pull_requests pr ON pr.issue_id = i.id
JOIN pr_review_requests rr ON rr.pr_issue_id = i.id
JOIN users u ON u.id = rr.requested_user_id
WHERE sr.id = $1
  AND sr.condition_review_requested = true
  AND sr.paused_at IS NULL
  AND r.deleted_at IS NULL
  AND r.is_archived = false
  AND rr.requested_user_id IS NOT NULL
  AND rr.dismissed_at IS NULL
  AND rr.satisfied_by_review_id IS NULL
  AND u.deleted_at IS NULL
  AND i.updated_at <= now() - (sr.min_age_minutes * interval '1 minute')
  AND (
    sr.target = 'all_repositories'
    OR (sr.target = 'repository' AND r.id = sr.repo_id)
    OR (
      sr.target = 'team'
      AND EXISTS (
        SELECT 1 FROM team_members tm
        WHERE tm.team_id = sr.team_id
          AND tm.user_id = rr.requested_user_id
      )
    )
  )
ORDER BY r.id ASC, i.number ASC, u.username ASC;

-- name: ListTeamReviewReminderCandidates :many
SELECT rr.id AS review_request_id,
       i.id AS pr_issue_id,
       i.number AS pr_number,
       i.title AS pr_title,
       i.author_user_id,
       r.id AS repo_id,
       r.owner_user_id,
       r.owner_org_id,
       r.visibility::text AS repo_visibility,
       r.is_archived,
       r.deleted_at,
       r.is_paused,
       u.id AS recipient_user_id,
       u.username AS recipient_username,
       (u.suspended_at IS NOT NULL)::bool AS recipient_suspended,
       u.is_site_admin AS recipient_site_admin
FROM org_scheduled_reminders sr
JOIN repos r ON r.owner_org_id = sr.org_id
JOIN issues i ON i.repo_id = r.id AND i.kind = 'pr' AND i.state = 'open'
JOIN pull_requests pr ON pr.issue_id = i.id
JOIN pr_review_requests rr ON rr.pr_issue_id = i.id
JOIN teams requested_team ON requested_team.id = rr.requested_team_id
JOIN team_members tm ON tm.team_id = requested_team.id
JOIN users u ON u.id = tm.user_id
WHERE sr.id = $1
  AND sr.condition_team_review_requested = true
  AND sr.paused_at IS NULL
  AND r.deleted_at IS NULL
  AND r.is_archived = false
  AND rr.requested_team_id IS NOT NULL
  AND requested_team.org_id = sr.org_id
  AND rr.dismissed_at IS NULL
  AND rr.satisfied_by_review_id IS NULL
  AND u.deleted_at IS NULL
  AND i.updated_at <= now() - (sr.min_age_minutes * interval '1 minute')
  AND (
    sr.target = 'all_repositories'
    OR (sr.target = 'repository' AND r.id = sr.repo_id)
    OR (sr.target = 'team' AND requested_team.id = sr.team_id)
  )
ORDER BY r.id ASC, i.number ASC, u.username ASC;
