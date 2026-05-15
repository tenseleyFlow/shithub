-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-05: vanity username reservations.

-- name: InsertUsernameReservation :one
INSERT INTO user_username_reservations (user_id, reserved_handle)
VALUES ($1, $2)
RETURNING *;

-- name: ListUsernameReservationsForUser :many
SELECT *
FROM user_username_reservations
WHERE user_id = $1
ORDER BY created_at;

-- name: CountUsernameReservationsForUser :one
SELECT count(*) FROM user_username_reservations WHERE user_id = $1;

-- name: DeleteUsernameReservation :exec
-- Scoped by user_id so a misaddressed request can't delete another
-- user's reservation by id-guess.
DELETE FROM user_username_reservations
WHERE id = $1 AND user_id = $2;

-- name: IsUsernameReservedByAnother :one
-- Returns true when the handle is reserved by a user OTHER than the
-- caller. Used at signup time (`except_user_id = 0` matches all rows)
-- and at rename time (the caller's own reservation is the one they're
-- about to convert, so it should not block them).
SELECT EXISTS (
    SELECT 1
    FROM user_username_reservations
    WHERE reserved_handle = sqlc.arg(reserved_handle)
      AND (sqlc.arg(except_user_id)::bigint = 0 OR user_id <> sqlc.arg(except_user_id))
) AS reserved;
