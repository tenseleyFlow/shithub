-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-07a: saved replies.

-- name: InsertSavedReply :one
INSERT INTO user_saved_replies (user_id, name, body)
VALUES ($1, $2, $3)
RETURNING *;

-- name: ListSavedRepliesForUser :many
SELECT *
FROM user_saved_replies
WHERE user_id = $1
ORDER BY name;

-- name: GetSavedReply :one
-- Scoped by user_id so an id-guess from another user is a no-op.
SELECT *
FROM user_saved_replies
WHERE id = $1 AND user_id = $2;

-- name: CountSavedRepliesForUser :one
SELECT count(*) FROM user_saved_replies WHERE user_id = $1;

-- name: UpdateSavedReply :exec
-- Scoped by user_id; updated_at refreshed on every write so the picker
-- can surface "most recently edited" if the UI wants to sort that way.
UPDATE user_saved_replies
SET name = $3, body = $4, updated_at = now()
WHERE id = $1 AND user_id = $2;

-- name: DeleteSavedReply :exec
-- Scoped by user_id; idempotent on missing rows.
DELETE FROM user_saved_replies
WHERE id = $1 AND user_id = $2;
