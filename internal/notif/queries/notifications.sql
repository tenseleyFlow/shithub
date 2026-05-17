-- ─── notifications ─────────────────────────────────────────────────

-- name: UpsertNotificationByThread :one
-- Coalesce-or-insert: if a row exists for (recipient, thread), bump
-- last_event_at + last_actor + reason and re-flip unread=true so the
-- inbox surfaces it again. Otherwise insert a fresh row.
--
-- Returns the resulting row (whether it was created or updated)
-- so the caller can chain an email-enqueue without a re-read.
INSERT INTO notifications (
    recipient_user_id, kind, reason, repo_id,
    thread_kind, thread_id, source_event_id,
    last_event_at, last_actor_user_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, now(), $8
)
ON CONFLICT (recipient_user_id, thread_kind, thread_id) WHERE thread_id IS NOT NULL
DO UPDATE SET
    kind               = EXCLUDED.kind,
    reason             = EXCLUDED.reason,
    source_event_id    = EXCLUDED.source_event_id,
    last_event_at      = now(),
    last_actor_user_id = EXCLUDED.last_actor_user_id,
    unread             = true,
    updated_at         = now()
RETURNING id, recipient_user_id, kind, reason, repo_id,
          thread_kind, thread_id, source_event_id, unread,
          last_event_at, last_actor_user_id, summary, created_at, updated_at,
          snoozed_until, tab_label, matched_rule_id;

-- name: InsertThreadlessNotification :one
-- For events with no thread (e.g. repo-admin lifecycle: archived).
-- These don't coalesce; each fires its own row. Used sparingly.
INSERT INTO notifications (
    recipient_user_id, kind, reason, repo_id,
    source_event_id, last_actor_user_id
) VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, recipient_user_id, kind, reason, repo_id,
          thread_kind, thread_id, source_event_id, unread,
          last_event_at, last_actor_user_id, summary, created_at, updated_at,
          snoozed_until, tab_label, matched_rule_id;

-- name: ListNotificationsForRecipient :many
-- Inbox view, recency-sorted. PRO-EXT01-16c extends the original
-- with:
--   * snoozed_until / tab_label / matched_rule_id from the rule
--     engine (PRO-EXT01-16a), so the template can render the badges.
--   * @tab_filter — when non-empty, restrict to that tab. Empty
--     string returns the default Inbox view (tab_label IS NULL or
--     awake snoozes), hiding snoozed-but-not-yet-due + dropped rows.
SELECT n.id, n.recipient_user_id, n.kind, n.reason, n.repo_id,
       n.thread_kind, n.thread_id, n.source_event_id, n.unread,
       n.last_event_at, n.last_actor_user_id, n.summary,
       n.created_at, n.updated_at,
       n.snoozed_until, n.tab_label, n.matched_rule_id,
       coalesce(u.username, '') AS actor_username,
       coalesce(r.name, '') AS repo_name,
       coalesce(ru.username, ro.slug, '') AS repo_owner_username,
       coalesce(i.number, 0) AS thread_number,
       coalesce(i.title, '') AS thread_title
FROM notifications n
LEFT JOIN users u  ON u.id = n.last_actor_user_id
LEFT JOIN repos r  ON r.id = n.repo_id
LEFT JOIN users ru ON ru.id = r.owner_user_id
LEFT JOIN orgs ro  ON ro.id = r.owner_org_id
LEFT JOIN issues i ON i.id = n.thread_id
WHERE n.recipient_user_id = $1
  AND ($2::boolean = false OR n.unread = true)
  AND (
    -- @tab_filter = '' → default Inbox view: hide dropped + hide
    -- snoozes that haven't yet woken up.
    ($5::text = '' AND (n.tab_label IS NULL OR n.tab_label <> 'dropped')
                 AND (n.snoozed_until IS NULL OR n.snoozed_until <= now()))
    -- Non-empty filter → exact tab match (works for 'dropped' too,
    -- so an investigation user can audit what their rules ate).
    OR ($5::text <> '' AND n.tab_label = $5::text)
  )
ORDER BY n.last_event_at DESC
LIMIT $3 OFFSET $4;

-- name: ListInboxTabsForRecipient :many
-- Surfaces the distinct tab labels the user has rules routing into,
-- with a count per tab. The inbox nav renders one chip per result so
-- the user can switch tabs. 'dropped' is excluded from the chip list
-- (it's auditable via ?tab=dropped but doesn't deserve nav real
-- estate by default).
SELECT tab_label::text AS label, count(*)::int AS count
FROM notifications
WHERE recipient_user_id = $1
  AND tab_label IS NOT NULL
  AND tab_label <> 'dropped'
GROUP BY tab_label
ORDER BY tab_label ASC;

-- name: CountUnreadForRecipient :one
-- Inbox badge count. PRO-EXT01-16c: matches the default-view filter
-- so a user with all their notifications snoozed sees a clean 0 in
-- the nav, not a misleading total that includes asleep rows.
SELECT count(*) FROM notifications
WHERE recipient_user_id = $1
  AND unread = true
  AND (tab_label IS NULL OR tab_label <> 'dropped')
  AND (snoozed_until IS NULL OR snoozed_until <= now());

-- name: CountNotificationsForRecipient :one
SELECT count(*) FROM notifications
WHERE recipient_user_id = $1
  AND ($2::boolean = false OR unread = true);

-- name: SetNotificationRead :exec
UPDATE notifications SET unread = false, updated_at = now()
WHERE id = $1 AND recipient_user_id = $2;

-- name: SetNotificationUnread :exec
UPDATE notifications SET unread = true, updated_at = now()
WHERE id = $1 AND recipient_user_id = $2;

-- name: MarkAllReadForRecipient :exec
-- Bounded sweep: a single call doesn't try to update millions of
-- rows. Caller paginates via repeated calls when count > batch.
UPDATE notifications SET unread = false, updated_at = now()
WHERE recipient_user_id = $1 AND unread = true;

-- name: GetNotification :one
SELECT id, recipient_user_id, kind, reason, repo_id,
       thread_kind, thread_id, source_event_id, unread,
       last_event_at, last_actor_user_id, summary, created_at, updated_at
FROM notifications WHERE id = $1;
