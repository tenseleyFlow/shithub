-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-10c: secret-scan allowlist queries.

-- name: InsertSecretScanAllowlist :one
-- Idempotent on (repo, pattern, path) — re-allowlisting an already-
-- allowlisted finding just updates the reason via the UPSERT.
INSERT INTO secret_scan_allowlist (repo_id, pattern, path, reason, created_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_id, pattern, path) DO UPDATE
SET reason = EXCLUDED.reason,
    created_by = EXCLUDED.created_by
RETURNING *;

-- name: ListSecretScanAllowlistForRepo :many
SELECT *
FROM secret_scan_allowlist
WHERE repo_id = $1
ORDER BY created_at DESC;

-- name: DeleteSecretScanAllowlist :exec
DELETE FROM secret_scan_allowlist
WHERE id = $1 AND repo_id = $2;

-- name: IsSecretScanAllowlisted :one
SELECT EXISTS (
    SELECT 1 FROM secret_scan_allowlist
    WHERE repo_id = $1 AND pattern = $2 AND path = $3
) AS allowlisted;

-- name: ApplyAllowlistToFindings :exec
-- Sweeps existing open findings against the allowlist after an entry
-- is added: any matching (pattern, path) flips to status='allowlisted'.
UPDATE secret_scan_findings
SET status = 'allowlisted', resolved_at = now()
WHERE repo_id = $1
  AND pattern = $2
  AND path = $3
  AND status = 'open';
