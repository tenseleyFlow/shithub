-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-10b: secret-scan findings storage.

-- name: UpsertSecretScanFinding :one
-- Re-scan idempotency: same (repo, pattern, path, line) hits the
-- unique constraint and we update last_seen_oid + last_seen_at
-- without changing status. New findings land at status='open'.
INSERT INTO secret_scan_findings (
    repo_id, pattern, path, line_no, excerpt, first_seen_oid, last_seen_oid
)
VALUES ($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT (repo_id, pattern, path, line_no) DO UPDATE
SET last_seen_oid = EXCLUDED.last_seen_oid,
    last_seen_at  = now()
RETURNING *;

-- name: ListSecretScanFindingsForRepo :many
-- Status-filterable list for the UI in 10c. Filter is optional;
-- empty string lists everything.
SELECT *
FROM secret_scan_findings
WHERE repo_id = $1
  AND (sqlc.arg(status_filter)::text = ''
       OR status::text = sqlc.arg(status_filter)::text)
ORDER BY status, last_seen_at DESC, id
LIMIT $2 OFFSET $3;

-- name: CountSecretScanFindingsForRepo :one
SELECT count(*) FROM secret_scan_findings
WHERE repo_id = $1
  AND (sqlc.arg(status_filter)::text = ''
       OR status::text = sqlc.arg(status_filter)::text);

-- name: GetSecretScanFinding :one
SELECT *
FROM secret_scan_findings
WHERE id = $1 AND repo_id = $2;

-- name: MarkSecretScanFindingsStaleForRepo :exec
-- After a rescan, any open finding whose last_seen_oid != current_oid
-- flips to 'stale'. Open status only — allowlisted / resolved rows
-- keep their status so the audit trail isn't lost.
UPDATE secret_scan_findings
SET status = 'stale', resolved_at = now()
WHERE repo_id = $1
  AND status = 'open'
  AND last_seen_oid <> $2;

-- name: ResolveSecretScanFinding :exec
UPDATE secret_scan_findings
SET status = 'resolved', resolved_at = now(), resolution_note = $3
WHERE id = $1 AND repo_id = $2;

-- name: OrgSecretScanSummary :one
WITH org_repos AS (
    SELECT id
    FROM repos
    WHERE owner_org_id = $1
      AND deleted_at IS NULL
)
SELECT
    count(*) FILTER (WHERE f.status = 'open')::bigint AS open_secret_count,
    count(*) FILTER (WHERE f.status = 'allowlisted')::bigint AS allowlisted_secret_count,
    count(*) FILTER (WHERE f.status = 'stale')::bigint AS stale_secret_count,
    count(*)::bigint AS total_secret_count,
    count(DISTINCT f.repo_id)::bigint AS affected_repo_count
FROM secret_scan_findings f
JOIN org_repos r ON r.id = f.repo_id;

-- name: ListOrgSecretScanFindings :many
SELECT
    f.id,
    f.repo_id,
    r.name AS repo_name,
    r.visibility AS repo_visibility,
    f.pattern,
    f.path,
    f.line_no,
    f.excerpt,
    f.status,
    f.first_seen_at,
    f.last_seen_at
FROM secret_scan_findings f
JOIN repos r ON r.id = f.repo_id
WHERE r.owner_org_id = $1
  AND r.deleted_at IS NULL
  AND f.status = 'open'
ORDER BY f.last_seen_at DESC, lower(r.name), lower(f.path), f.line_no
LIMIT $2 OFFSET $3;
