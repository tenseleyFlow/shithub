-- ─── notification_email_log ────────────────────────────────────────

-- name: InsertEmailLog :exec
-- Records an email send. Caller decides what to bind for thread_id
-- (NULL for thread-less notifications). MessageID is the SMTP /
-- transactional-provider message id when available; empty when
-- the sender doesn't surface one.
INSERT INTO notification_email_log
    (recipient_user_id, notification_id, thread_kind, thread_id, message_id)
VALUES ($1, $2, $3, $4, $5);

-- name: CountEmailsForRecipientThreadSince :one
-- Storm dampener probe: how many emails for this thread did we
-- send to this recipient in the last $4 minutes? Caller compares
-- to the cap.
SELECT count(*) FROM notification_email_log
WHERE recipient_user_id = $1
  AND thread_kind = $2
  AND thread_id = $3
  AND sent_at > now() - make_interval(mins => $4::int);

-- name: CountEmailsForRecipientSince :one
-- Per-recipient absolute rate cap: how many total emails to this
-- recipient in the last $2 minutes?
SELECT count(*) FROM notification_email_log
WHERE recipient_user_id = $1
  AND sent_at > now() - make_interval(mins => $2::int);
