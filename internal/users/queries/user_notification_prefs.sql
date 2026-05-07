-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: ListUserNotificationPrefs :many
SELECT user_id, key, value, updated_at
FROM user_notification_prefs
WHERE user_id = $1
ORDER BY key;

-- name: UpsertUserNotificationPref :exec
INSERT INTO user_notification_prefs (user_id, key, value)
VALUES ($1, $2, $3)
ON CONFLICT (user_id, key) DO UPDATE
SET value = EXCLUDED.value;

-- name: DeleteUserNotificationPref :exec
DELETE FROM user_notification_prefs WHERE user_id = $1 AND key = $2;
