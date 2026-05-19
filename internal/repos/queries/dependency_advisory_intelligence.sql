-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertDependencyAdvisorySource :one
INSERT INTO dependency_advisory_sources (
    name, kind, display_name, url, license, attribution,
    enabled, last_sync_status, last_sync_error, cursor_value, etag, metadata
) VALUES (
    sqlc.arg(name)::text,
    sqlc.arg(kind)::text,
    sqlc.arg(display_name)::text,
    sqlc.arg(url)::text,
    sqlc.arg(license)::text,
    sqlc.arg(attribution)::text,
    sqlc.arg(enabled)::boolean,
    sqlc.arg(last_sync_status)::text,
    sqlc.arg(last_sync_error)::text,
    sqlc.arg(cursor_value)::text,
    sqlc.arg(etag)::text,
    sqlc.arg(metadata)::jsonb
)
ON CONFLICT (name) DO UPDATE
SET kind = EXCLUDED.kind,
    display_name = EXCLUDED.display_name,
    url = EXCLUDED.url,
    license = EXCLUDED.license,
    attribution = EXCLUDED.attribution,
    enabled = EXCLUDED.enabled,
    last_sync_status = EXCLUDED.last_sync_status,
    last_sync_error = EXCLUDED.last_sync_error,
    cursor_value = EXCLUDED.cursor_value,
    etag = EXCLUDED.etag,
    metadata = EXCLUDED.metadata
RETURNING *;

-- name: GetDependencyAdvisorySource :one
SELECT *
FROM dependency_advisory_sources
WHERE name = $1;

-- name: ListDependencyAdvisorySources :many
SELECT *
FROM dependency_advisory_sources
ORDER BY enabled DESC, name;

-- name: MarkDependencyAdvisorySourceSync :exec
UPDATE dependency_advisory_sources
SET last_sync_at = now(),
    last_sync_status = $2,
    last_sync_error = $3,
    cursor_value = $4,
    etag = $5
WHERE name = $1;

-- name: StartDependencyAdvisorySyncRun :one
INSERT INTO dependency_advisory_sync_runs (
    source_name, status, metadata
) VALUES (
    $1, 'running', sqlc.arg(metadata)::jsonb
)
RETURNING *;

-- name: FinishDependencyAdvisorySyncRun :one
UPDATE dependency_advisory_sync_runs
SET status = $2,
    finished_at = now(),
    advisory_count = $3,
    upserted_count = $4,
    withdrawn_count = $5,
    error_message = $6,
    metadata = $7
WHERE id = $1
RETURNING *;

-- name: UpsertDependencyAdvisoryWithMetadata :one
INSERT INTO dependency_advisories (
    source, external_id, ecosystem, package_name, affected_range,
    patched_versions, severity, summary, description, reference_urls,
    published_at, withdrawn_at, modified_at, source_url, cvss_score,
    cvss_vector, cwe_ids
) VALUES (
    sqlc.arg(source)::text,
    sqlc.arg(external_id)::text,
    sqlc.arg(ecosystem)::text,
    sqlc.arg(package_name)::text,
    sqlc.arg(affected_range)::text,
    sqlc.arg(patched_versions)::text,
    sqlc.arg(severity)::text,
    sqlc.arg(summary)::text,
    sqlc.arg(description)::text,
    sqlc.arg(reference_urls)::jsonb,
    sqlc.narg(published_at)::timestamptz,
    sqlc.narg(withdrawn_at)::timestamptz,
    sqlc.narg(modified_at)::timestamptz,
    sqlc.arg(source_url)::text,
    sqlc.narg(cvss_score)::numeric,
    sqlc.arg(cvss_vector)::text,
    sqlc.arg(cwe_ids)::jsonb
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
    withdrawn_at = EXCLUDED.withdrawn_at,
    modified_at = EXCLUDED.modified_at,
    source_url = EXCLUDED.source_url,
    cvss_score = EXCLUDED.cvss_score,
    cvss_vector = EXCLUDED.cvss_vector,
    cwe_ids = EXCLUDED.cwe_ids
RETURNING *;

-- name: GetDependencyAdvisoryBySourceExternalID :one
SELECT *
FROM dependency_advisories
WHERE source = $1
  AND external_id = $2;

-- name: DeleteDependencyAdvisoryAliases :exec
DELETE FROM dependency_advisory_aliases
WHERE advisory_id = $1;

-- name: InsertDependencyAdvisoryAlias :exec
INSERT INTO dependency_advisory_aliases (
    advisory_id, alias_kind, alias_value
) VALUES (
    $1, $2, $3
)
ON CONFLICT (advisory_id, alias_kind, lower(alias_value)) DO NOTHING;

-- name: ListDependencyAdvisoryAliases :many
SELECT *
FROM dependency_advisory_aliases
WHERE advisory_id = $1
ORDER BY alias_kind, alias_value;

-- name: DeleteDependencyAdvisoryAffectedRanges :exec
DELETE FROM dependency_advisory_affected_ranges
WHERE advisory_id = $1;

-- name: InsertDependencyAdvisoryAffectedRange :exec
INSERT INTO dependency_advisory_affected_ranges (
    advisory_id, ecosystem, package_name, range_expression,
    introduced, fixed, last_affected, metadata
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, sqlc.arg(metadata)::jsonb
)
ON CONFLICT (
    advisory_id, ecosystem, lower(package_name),
    range_expression, introduced, fixed, last_affected
) DO UPDATE
SET metadata = EXCLUDED.metadata;

-- name: ListDependencyAdvisoryAffectedRanges :many
SELECT *
FROM dependency_advisory_affected_ranges
WHERE advisory_id = $1
ORDER BY ecosystem, lower(package_name), range_expression, introduced, fixed, last_affected;
