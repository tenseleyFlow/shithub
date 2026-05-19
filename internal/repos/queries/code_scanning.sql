-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP27 code scanning / SARIF upload queries.

-- name: CreateCodeScanningUpload :one
INSERT INTO code_scanning_uploads (
    repo_id, tool_name, tool_guid, category, commit_sha, ref_name,
    alert_count, raw_sarif_sha256, uploaded_by
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, sqlc.narg(uploaded_by)::bigint
)
RETURNING *;

-- name: UpsertCodeScanningAlert :one
INSERT INTO code_scanning_alerts (
    repo_id, tool_name, tool_guid, rule_id, rule_name, severity,
    message, path, start_line, end_line, start_column, end_column,
    fingerprint, commit_sha, ref_name
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11, $12,
    $13, $14, $15
)
ON CONFLICT (repo_id, tool_name, rule_id, path, start_line, fingerprint) DO UPDATE
SET tool_guid = EXCLUDED.tool_guid,
    rule_name = EXCLUDED.rule_name,
    severity = EXCLUDED.severity,
    message = EXCLUDED.message,
    end_line = EXCLUDED.end_line,
    start_column = EXCLUDED.start_column,
    end_column = EXCLUDED.end_column,
    commit_sha = EXCLUDED.commit_sha,
    ref_name = EXCLUDED.ref_name,
    status = CASE
        WHEN code_scanning_alerts.status = 'dismissed' THEN code_scanning_alerts.status
        ELSE 'open'
    END,
    fixed_at = CASE
        WHEN code_scanning_alerts.status = 'dismissed' THEN code_scanning_alerts.fixed_at
        ELSE NULL
    END,
    last_seen_at = now()
RETURNING *;

-- name: GetCodeScanningAlert :one
SELECT *
FROM code_scanning_alerts
WHERE id = $1 AND repo_id = $2;

-- name: ListCodeScanningAlertsForRepo :many
SELECT *
FROM code_scanning_alerts
WHERE repo_id = $1
  AND (sqlc.arg(status_filter)::text = ''
       OR status = sqlc.arg(status_filter)::text)
ORDER BY
    CASE severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        ELSE 3
    END,
    last_seen_at DESC,
    lower(path),
    start_line,
    lower(rule_id)
LIMIT $2 OFFSET $3;

-- name: CodeScanningSummaryForRepo :one
SELECT
    count(*) FILTER (WHERE status = 'open')::bigint AS open_alert_count,
    count(*) FILTER (WHERE status = 'dismissed')::bigint AS dismissed_alert_count,
    count(*) FILTER (WHERE status = 'fixed')::bigint AS fixed_alert_count,
    count(*) FILTER (WHERE status = 'open' AND severity = 'critical')::bigint AS critical_alert_count,
    count(*) FILTER (WHERE status = 'open' AND severity = 'high')::bigint AS high_alert_count,
    count(*)::bigint AS total_alert_count
FROM code_scanning_alerts
WHERE repo_id = $1;

-- name: DismissCodeScanningAlert :exec
UPDATE code_scanning_alerts
SET status = 'dismissed',
    dismissal_note = $3,
    dismissed_by = sqlc.narg(dismissed_by)::bigint,
    dismissed_at = now(),
    fixed_at = NULL
WHERE id = $1
  AND repo_id = $2;

-- name: ReopenCodeScanningAlert :exec
UPDATE code_scanning_alerts
SET status = 'open',
    dismissal_note = '',
    dismissed_by = NULL,
    dismissed_at = NULL,
    fixed_at = NULL
WHERE id = $1
  AND repo_id = $2;

-- name: OrgCodeScanningSummary :one
WITH org_repos AS (
    SELECT id
    FROM repos
    WHERE owner_org_id = $1
      AND deleted_at IS NULL
),
open_alerts AS (
    SELECT a.*
    FROM code_scanning_alerts a
    JOIN org_repos r ON r.id = a.repo_id
    WHERE a.status = 'open'
)
SELECT
    count(*)::bigint AS open_code_alert_count,
    count(*) FILTER (WHERE severity = 'critical')::bigint AS critical_code_alert_count,
    count(*) FILTER (WHERE severity = 'high')::bigint AS high_code_alert_count,
    count(DISTINCT repo_id)::bigint AS affected_repo_count
FROM open_alerts;

-- name: ListOrgCodeScanningAlerts :many
SELECT
    a.id,
    a.repo_id,
    r.name AS repo_name,
    r.visibility AS repo_visibility,
    a.tool_name,
    a.rule_id,
    a.rule_name,
    a.severity,
    a.message,
    a.path,
    a.start_line,
    a.fingerprint,
    a.commit_sha,
    a.ref_name,
    a.first_seen_at,
    a.last_seen_at
FROM code_scanning_alerts a
JOIN repos r ON r.id = a.repo_id
WHERE r.owner_org_id = $1
  AND r.deleted_at IS NULL
  AND a.status = 'open'
ORDER BY
    CASE a.severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        ELSE 3
    END,
    a.last_seen_at DESC,
    lower(r.name),
    lower(a.path),
    a.start_line
LIMIT $2 OFFSET $3;

-- name: CreateCodeSecurityCampaign :one
INSERT INTO code_security_campaigns (
    repo_id, title, description, created_by
) VALUES (
    $1, $2, $3, sqlc.narg(created_by)::bigint
)
RETURNING *;

-- name: ListCodeSecurityCampaignsForRepo :many
SELECT
    c.*,
    COALESCE(alert_counts.alert_count, 0)::bigint AS alert_count,
    COALESCE(alert_counts.open_alert_count, 0)::bigint AS open_alert_count
FROM code_security_campaigns c
LEFT JOIN LATERAL (
    SELECT
        count(*) AS alert_count,
        count(*) FILTER (WHERE a.status = 'open') AS open_alert_count
    FROM code_security_campaign_alerts ca
    JOIN code_scanning_alerts a ON a.id = ca.alert_id
    WHERE ca.campaign_id = c.id
) alert_counts ON true
WHERE c.repo_id = $1
ORDER BY
    CASE c.state WHEN 'open' THEN 0 ELSE 1 END,
    c.updated_at DESC;

-- name: AddCodeSecurityCampaignAlert :exec
INSERT INTO code_security_campaign_alerts (campaign_id, alert_id)
SELECT $1, a.id
FROM code_scanning_alerts a
JOIN code_security_campaigns c ON c.id = $1
WHERE a.id = $2
  AND a.repo_id = c.repo_id
ON CONFLICT DO NOTHING;

-- name: CloseCodeSecurityCampaign :exec
UPDATE code_security_campaigns
SET state = 'closed',
    closed_at = now()
WHERE id = $1
  AND repo_id = $2;

-- name: ReopenCodeSecurityCampaign :exec
UPDATE code_security_campaigns
SET state = 'open',
    closed_at = NULL
WHERE id = $1
  AND repo_id = $2;
