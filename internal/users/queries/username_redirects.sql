-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: LookupUsernameRedirect :one
-- Resolve an old username to the current username via the user_id FK.
-- Returns ErrNoRows when no redirect exists.
SELECT u.username, r.changed_at
FROM username_redirects r
JOIN users u ON u.id = r.user_id
WHERE r.old_username = $1 AND u.deleted_at IS NULL;

-- name: InsertUsernameRedirect :exec
-- Used by the S10 username-change flow to record an old name. The
-- redirect itself doubles as a 30-day reservation (the row stays for at
-- least that long).
INSERT INTO username_redirects (old_username, user_id) VALUES ($1, $2);
