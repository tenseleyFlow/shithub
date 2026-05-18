-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25 repository dependency inventory, dependency alerts, and
-- repository security advisories.

-- name: UpsertRepoDependencySnapshot :one
INSERT INTO repo_dependency_snapshots (
    repo_id, default_branch, head_sha, manifest_count, dependency_count, generated_at
) VALUES (
    $1, $2, $3, $4, $5, now()
)
ON CONFLICT (repo_id) DO UPDATE
SET default_branch = EXCLUDED.default_branch,
    head_sha = EXCLUDED.head_sha,
    manifest_count = EXCLUDED.manifest_count,
    dependency_count = EXCLUDED.dependency_count,
    generated_at = now()
RETURNING *;

-- name: GetRepoDependencySnapshot :one
SELECT *
FROM repo_dependency_snapshots
WHERE repo_id = $1;

-- name: UpsertRepoDependency :one
INSERT INTO repo_dependencies (
    repo_id, ecosystem, package_name, package_version, manifest_path,
    lockfile_path, scope, direct, package_manager, source, last_seen_sha
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10, $11
)
ON CONFLICT (repo_id, ecosystem, package_name, manifest_path) DO UPDATE
SET package_version = EXCLUDED.package_version,
    lockfile_path = EXCLUDED.lockfile_path,
    scope = EXCLUDED.scope,
    direct = EXCLUDED.direct,
    package_manager = EXCLUDED.package_manager,
    source = EXCLUDED.source,
    last_seen_sha = EXCLUDED.last_seen_sha,
    last_seen_at = now(),
    stale_at = NULL
RETURNING *;

-- name: MarkRepoDependenciesStale :exec
UPDATE repo_dependencies
SET stale_at = now()
WHERE repo_id = $1
  AND last_seen_sha <> $2
  AND stale_at IS NULL;

-- name: ListRepoDependenciesForRepo :many
SELECT *
FROM repo_dependencies
WHERE repo_id = $1
  AND (sqlc.arg(include_stale)::boolean OR stale_at IS NULL)
ORDER BY ecosystem, lower(package_name), manifest_path;

-- name: UpsertDependencyAdvisory :one
INSERT INTO dependency_advisories (
    source, external_id, ecosystem, package_name, affected_range,
    patched_versions, severity, summary, description, reference_urls,
    published_at, withdrawn_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, $12
)
ON CONFLICT (source, external_id) DO UPDATE
SET ecosystem = EXCLUDED.ecosystem,
    package_name = EXCLUDED.package_name,
    affected_range = EXCLUDED.affected_range,
    patched_versions = EXCLUDED.patched_versions,
    severity = EXCLUDED.severity,
    summary = EXCLUDED.summary,
    description = EXCLUDED.description,
    reference_urls = EXCLUDED.reference_urls,
    published_at = EXCLUDED.published_at,
    withdrawn_at = EXCLUDED.withdrawn_at
RETURNING *;

-- name: RefreshDependencyAlertsForRepo :exec
-- Baseline matcher: advisories match exact package/ecosystem plus
-- affected_range of '', '*', or the dependency's resolved version.
-- Rich semver range evaluation belongs in a later parser package; this
-- keeps SP25 honest about what it can safely claim.
INSERT INTO repo_dependency_alerts (
    repo_id, dependency_id, advisory_id, status, last_seen_at
)
SELECT d.repo_id, d.id, a.id, 'open', now()
FROM repo_dependencies d
JOIN dependency_advisories a
  ON lower(a.ecosystem) = lower(d.ecosystem)
 AND lower(a.package_name) = lower(d.package_name)
WHERE d.repo_id = $1
  AND d.stale_at IS NULL
  AND a.withdrawn_at IS NULL
  AND (a.affected_range = '' OR a.affected_range = '*' OR a.affected_range = d.package_version)
ON CONFLICT (repo_id, dependency_id, advisory_id) DO UPDATE
SET last_seen_at = now(),
    status = CASE
        WHEN repo_dependency_alerts.status = 'resolved' THEN 'open'
        ELSE repo_dependency_alerts.status
    END,
    resolved_at = CASE
        WHEN repo_dependency_alerts.status = 'resolved' THEN NULL
        ELSE repo_dependency_alerts.resolved_at
    END;

-- name: ResolveStaleDependencyAlertsForRepo :exec
UPDATE repo_dependency_alerts alert
SET status = 'resolved',
    resolved_at = now(),
    last_seen_at = now()
WHERE alert.repo_id = $1
  AND alert.status = 'open'
  AND NOT EXISTS (
      SELECT 1
      FROM repo_dependencies d
      JOIN dependency_advisories a ON a.id = alert.advisory_id
      WHERE d.id = alert.dependency_id
        AND d.repo_id = alert.repo_id
        AND d.stale_at IS NULL
        AND a.withdrawn_at IS NULL
        AND (a.affected_range = '' OR a.affected_range = '*' OR a.affected_range = d.package_version)
  );

-- name: DismissDependencyAlert :exec
UPDATE repo_dependency_alerts
SET status = 'dismissed',
    dismissal_note = $3,
    dismissed_by = sqlc.narg(dismissed_by)::bigint,
    dismissed_at = now(),
    resolved_at = NULL,
    last_seen_at = now()
WHERE id = $1
  AND repo_id = $2;

-- name: ListOpenDependencyAlertsForRepo :many
SELECT
    alert.id,
    alert.repo_id,
    d.ecosystem,
    d.package_name,
    d.package_version,
    d.manifest_path,
    a.source,
    a.external_id,
    a.severity,
    a.summary,
    a.patched_versions,
    alert.first_seen_at,
    alert.last_seen_at
FROM repo_dependency_alerts alert
JOIN repo_dependencies d ON d.id = alert.dependency_id
JOIN dependency_advisories a ON a.id = alert.advisory_id
WHERE alert.repo_id = $1
  AND alert.status = 'open'
ORDER BY
    CASE a.severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        ELSE 3
    END,
    alert.last_seen_at DESC,
    lower(d.package_name);

-- name: OrgSecurityOverviewSummary :one
WITH org_repos AS (
    SELECT id
    FROM repos
    WHERE owner_org_id = $1
      AND deleted_at IS NULL
),
current_deps AS (
    SELECT d.*
    FROM repo_dependencies d
    JOIN org_repos r ON r.id = d.repo_id
    WHERE d.stale_at IS NULL
),
open_alerts AS (
    SELECT alert.*, a.severity
    FROM repo_dependency_alerts alert
    JOIN dependency_advisories a ON a.id = alert.advisory_id
    JOIN current_deps d ON d.id = alert.dependency_id
    WHERE alert.status = 'open'
),
repo_advisories AS (
    SELECT rsa.*
    FROM repo_security_advisories rsa
    JOIN org_repos r ON r.id = rsa.repo_id
    WHERE rsa.state IN ('draft', 'published')
)
SELECT
    (SELECT count(*) FROM org_repos) AS repo_count,
    (SELECT count(*) FROM current_deps) AS dependency_count,
    (SELECT count(DISTINCT manifest_path) FROM current_deps) AS manifest_count,
    (SELECT count(*) FROM open_alerts) AS open_alert_count,
    (SELECT count(*) FROM open_alerts WHERE severity = 'critical') AS critical_alert_count,
    (SELECT count(*) FROM open_alerts WHERE severity = 'high') AS high_alert_count,
    (SELECT count(*) FROM open_alerts WHERE severity = 'moderate') AS moderate_alert_count,
    (SELECT count(*) FROM repo_advisories) AS repository_advisory_count;

-- name: ListOrgDependencyAlerts :many
SELECT
    alert.id AS alert_id,
    r.id AS repo_id,
    r.name AS repo_name,
    r.visibility AS repo_visibility,
    d.ecosystem,
    d.package_name,
    d.package_version,
    d.manifest_path,
    a.source,
    a.external_id,
    a.severity,
    a.summary,
    a.patched_versions,
    alert.first_seen_at,
    alert.last_seen_at
FROM repo_dependency_alerts alert
JOIN repo_dependencies d ON d.id = alert.dependency_id
JOIN dependency_advisories a ON a.id = alert.advisory_id
JOIN repos r ON r.id = alert.repo_id
WHERE r.owner_org_id = $1
  AND r.deleted_at IS NULL
  AND d.stale_at IS NULL
  AND alert.status = 'open'
ORDER BY
    CASE a.severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        ELSE 3
    END,
    alert.last_seen_at DESC,
    lower(r.name),
    lower(d.package_name)
LIMIT $2 OFFSET $3;

-- name: ListOrgSecurityRepoSummaries :many
SELECT
    r.id AS repo_id,
    r.name AS repo_name,
    r.visibility AS repo_visibility,
    COALESCE(s.dependency_count, 0)::bigint AS dependency_count,
    COALESCE(s.manifest_count, 0)::bigint AS manifest_count,
    COALESCE(alerts.open_alert_count, 0)::bigint AS open_alert_count,
    COALESCE(advisories.repository_advisory_count, 0)::bigint AS repository_advisory_count,
    s.generated_at AS last_scanned_at
FROM repos r
LEFT JOIN repo_dependency_snapshots s ON s.repo_id = r.id
LEFT JOIN LATERAL (
    SELECT count(*) AS open_alert_count
    FROM repo_dependency_alerts alert
    JOIN repo_dependencies d ON d.id = alert.dependency_id
    WHERE alert.repo_id = r.id
      AND alert.status = 'open'
      AND d.stale_at IS NULL
) alerts ON true
LEFT JOIN LATERAL (
    SELECT count(*) AS repository_advisory_count
    FROM repo_security_advisories rsa
    WHERE rsa.repo_id = r.id
      AND rsa.state IN ('draft', 'published')
) advisories ON true
WHERE r.owner_org_id = $1
  AND r.deleted_at IS NULL
ORDER BY open_alert_count DESC, lower(r.name)
LIMIT $2 OFFSET $3;

-- name: CreateRepoSecurityAdvisory :one
INSERT INTO repo_security_advisories (
    repo_id, identifier, state, severity, summary, description,
    affected_ecosystem, affected_package, vulnerable_versions,
    patched_versions, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, sqlc.narg(created_by)::bigint
)
RETURNING *;

-- name: ListRepoSecurityAdvisories :many
SELECT *
FROM repo_security_advisories
WHERE repo_id = $1
ORDER BY
    CASE state
        WHEN 'draft' THEN 0
        WHEN 'published' THEN 1
        ELSE 2
    END,
    updated_at DESC;

-- name: ListOrgSecurityAdvisories :many
SELECT
    rsa.id,
    rsa.repo_id,
    r.name AS repo_name,
    r.visibility AS repo_visibility,
    rsa.identifier,
    rsa.state,
    rsa.severity,
    rsa.summary,
    rsa.affected_ecosystem,
    rsa.affected_package,
    rsa.vulnerable_versions,
    rsa.patched_versions,
    rsa.updated_at
FROM repo_security_advisories rsa
JOIN repos r ON r.id = rsa.repo_id
WHERE r.owner_org_id = $1
  AND r.deleted_at IS NULL
ORDER BY
    CASE rsa.state
        WHEN 'draft' THEN 0
        WHEN 'published' THEN 1
        ELSE 2
    END,
    rsa.updated_at DESC
LIMIT $2 OFFSET $3;
