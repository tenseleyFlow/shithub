-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-16b: scheduled digest emails.

-- name: GetUserNotificationDigest :one
SELECT user_id, enabled, frequency, hour_utc, day_of_week,
       last_sent_at, next_send_at, created_at, updated_at
FROM user_notification_digests
WHERE user_id = $1;

-- name: UpsertUserNotificationDigest :one
-- Settings save: insert or update the user's single digest row.
-- next_send_at is supplied by the handler (which computes the next
-- tick given frequency + hour + DOW).
INSERT INTO user_notification_digests (
    user_id, enabled, frequency, hour_utc, day_of_week, next_send_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg(day_of_week)::smallint, sqlc.narg(next_send_at)::timestamptz
)
ON CONFLICT (user_id) DO UPDATE SET
    enabled      = EXCLUDED.enabled,
    frequency    = EXCLUDED.frequency,
    hour_utc     = EXCLUDED.hour_utc,
    day_of_week  = EXCLUDED.day_of_week,
    next_send_at = EXCLUDED.next_send_at,
    updated_at   = now()
RETURNING user_id, enabled, frequency, hour_utc, day_of_week,
          last_sent_at, next_send_at, created_at, updated_at;

-- name: DeleteUserNotificationDigest :exec
DELETE FROM user_notification_digests WHERE user_id = $1;

-- name: ClaimDueNotificationDigests :many
-- Sweep claim. Atomic claim-by-advance: each due row's
-- next_send_at is bumped one hour into the future as part of the
-- claim, so a second worker running the same statement sees the row
-- as "not due" and skips it. FOR UPDATE SKIP LOCKED inside the CTE
-- guards against two concurrent claims racing on the SELECT step.
--
-- Failure handling: if the worker dies between claim and the
-- success-path AdvanceUserNotificationDigest, the row sits with
-- next_send_at = now()+1h and gets re-picked-up automatically by
-- the next sweep tick after that window. The user-visible failure
-- mode is a one-hour delay on one digest send, which is better than
-- the previous race window where two workers could double-send.
-- PRO-EXT_SR2-11 (audit H3).
WITH due AS (
    SELECT user_id
    FROM user_notification_digests
    WHERE enabled = true
      AND next_send_at IS NOT NULL
      AND next_send_at <= now()
    ORDER BY next_send_at ASC
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE user_notification_digests d
SET next_send_at = now() + interval '1 hour',
    updated_at   = now()
FROM due
WHERE d.user_id = due.user_id
RETURNING d.user_id, d.enabled, d.frequency, d.hour_utc, d.day_of_week,
          d.last_sent_at, d.next_send_at, d.created_at, d.updated_at;

-- name: AdvanceUserNotificationDigest :exec
-- After a successful send, bump the cursor + record last_sent_at.
-- The handler computes the next tick (frequency + hour + DOW) so
-- the SQL stays plan-stable.
UPDATE user_notification_digests
SET last_sent_at = now(),
    next_send_at = $2,
    updated_at   = now()
WHERE user_id = $1;

-- name: ListUnreadNotificationsForDigest :many
-- Body source for the digest: unread, not snoozed past the send
-- window, not dropped. Limited to the most recent 50 so a user with
-- a giant unread backlog gets a manageable email.
SELECT n.id, n.recipient_user_id, n.kind, n.reason, n.repo_id,
       n.thread_kind, n.thread_id, n.source_event_id, n.unread,
       n.last_event_at, n.last_actor_user_id, n.summary,
       n.created_at, n.updated_at,
       n.snoozed_until, n.tab_label, n.matched_rule_id,
       coalesce(u.username, '') AS actor_username,
       coalesce(r.name, '') AS repo_name,
       coalesce(ru.username, '') AS repo_owner_username,
       coalesce(i.number, 0) AS thread_number,
       coalesce(i.title, '') AS thread_title
FROM notifications n
LEFT JOIN users u  ON u.id = n.last_actor_user_id
LEFT JOIN repos r  ON r.id = n.repo_id
LEFT JOIN users ru ON ru.id = r.owner_user_id
LEFT JOIN issues i ON i.id = n.thread_id
WHERE n.recipient_user_id = $1
  AND n.unread = true
  AND (n.snoozed_until IS NULL OR n.snoozed_until <= now())
  AND (n.tab_label IS NULL OR n.tab_label <> 'dropped')
ORDER BY n.last_event_at DESC
LIMIT 50;
