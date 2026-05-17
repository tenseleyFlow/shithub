-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-16a: notification routing rules CRUD + fanout-side load.
--
-- Read paths are by-user (settings list, fanout evaluation); write
-- paths are by-id with a user_id guard so a leaked id can't be
-- mutated cross-user.

-- name: ListUserNotificationRules :many
-- Settings page listing. All rules (enabled + disabled) so the user
-- can toggle them. Position-ordered so the UI reflects evaluation
-- order even when some rules are disabled.
SELECT id, user_id, name, enabled, position,
       match_reason, match_kind, match_repo_id, match_actor_id,
       action, action_tab, action_snooze_minutes,
       created_at, updated_at
FROM user_notification_rules
WHERE user_id = $1
ORDER BY position ASC, id ASC;

-- name: ListEnabledUserNotificationRules :many
-- Fanout-side load: just the active rules in evaluation order. The
-- `user_notification_rules_user_enabled_idx` partial index makes this
-- a single index seek.
SELECT id, user_id, name, enabled, position,
       match_reason, match_kind, match_repo_id, match_actor_id,
       action, action_tab, action_snooze_minutes,
       created_at, updated_at
FROM user_notification_rules
WHERE user_id = $1 AND enabled = true
ORDER BY position ASC, id ASC;

-- name: GetUserNotificationRule :one
-- By-id read with user_id guard. Empty result on cross-user access
-- forces the handler to 404 — no existence leak.
SELECT id, user_id, name, enabled, position,
       match_reason, match_kind, match_repo_id, match_actor_id,
       action, action_tab, action_snooze_minutes,
       created_at, updated_at
FROM user_notification_rules
WHERE id = $1 AND user_id = $2;

-- name: NextUserNotificationRulePosition :one
-- Returns max(position)+1 for the user, or 0 if no rules exist.
-- Used by insert to keep new rules at the end of the evaluation
-- order; the user can reorder later.
SELECT COALESCE(MAX(position) + 1, 0)::int AS next_position
FROM user_notification_rules
WHERE user_id = $1;

-- name: InsertUserNotificationRule :one
INSERT INTO user_notification_rules (
    user_id, name, enabled, position,
    match_reason, match_kind, match_repo_id, match_actor_id,
    action, action_tab, action_snooze_minutes
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg(match_reason)::text,
    sqlc.narg(match_kind)::text,
    sqlc.narg(match_repo_id)::bigint,
    sqlc.narg(match_actor_id)::bigint,
    $5,
    sqlc.narg(action_tab)::text,
    sqlc.narg(action_snooze_minutes)::int
)
RETURNING id, user_id, name, enabled, position,
          match_reason, match_kind, match_repo_id, match_actor_id,
          action, action_tab, action_snooze_minutes,
          created_at, updated_at;

-- name: DeleteUserNotificationRule :execrows
-- execrows lets the handler distinguish "deleted" from "not found"
-- without a prior GET.
DELETE FROM user_notification_rules
WHERE id = $1 AND user_id = $2;

-- name: SetUserNotificationRuleEnabled :execrows
UPDATE user_notification_rules
SET enabled = $3, updated_at = now()
WHERE id = $1 AND user_id = $2;

-- ─── notification mutation queries (fanout-side apply) ─────────────

-- name: ApplyRuleSnooze :exec
-- Stamp snoozed_until + matched_rule_id on a freshly-upserted
-- notification. Idempotent shape — re-running over the same row
-- with the same rule has the same effect.
UPDATE notifications
SET snoozed_until   = $2,
    matched_rule_id = $3,
    updated_at      = now()
WHERE id = $1;

-- name: ApplyRuleTab :exec
UPDATE notifications
SET tab_label       = $2,
    matched_rule_id = $3,
    updated_at      = now()
WHERE id = $1;

-- name: ApplyRuleMarkRead :exec
UPDATE notifications
SET unread          = false,
    matched_rule_id = $2,
    updated_at      = now()
WHERE id = $1;

-- name: ApplyRuleDrop :exec
-- "drop" hides via a dedicated tab label; we still keep the row so
-- the user can find it if a rule misfires.
UPDATE notifications
SET tab_label       = 'dropped',
    matched_rule_id = $2,
    updated_at      = now()
WHERE id = $1;
