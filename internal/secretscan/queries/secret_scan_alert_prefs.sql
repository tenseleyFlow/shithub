-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-10d: per-user alert preference CRUD.

-- name: GetSecretScanAlertPrefs :one
-- Returns the row or pgx.ErrNoRows; the handler treats absence as "no
-- alerts configured" so the absence is the off state.
SELECT *
FROM secret_scan_alert_prefs
WHERE user_id = $1;

-- name: UpsertSecretScanAlertPrefs :one
-- Settings handler calls this. The DB CHECK constraints reject the
-- malformed configurations (webhook url without secret, no enabled
-- channel) so the handler doesn't have to repeat that validation.
INSERT INTO secret_scan_alert_prefs (
    user_id, email_enabled, webhook_url, webhook_secret
)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE
SET email_enabled  = EXCLUDED.email_enabled,
    webhook_url    = EXCLUDED.webhook_url,
    webhook_secret = EXCLUDED.webhook_secret,
    updated_at     = now()
RETURNING *;

-- name: DeleteSecretScanAlertPrefs :exec
-- The all-channels-off case maps to row deletion rather than
-- preserving an empty row (the at_least_one check would reject it).
DELETE FROM secret_scan_alert_prefs WHERE user_id = $1;

-- name: TouchSecretScanAlertPrefsAlertedAt :exec
-- Recorded after a successful send so the worker can dedupe re-scans
-- that re-surface a known finding (status=open → stale → open again).
UPDATE secret_scan_alert_prefs
SET last_alerted_at = now()
WHERE user_id = $1;
