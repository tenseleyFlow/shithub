-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertPullDependencyReview :one
INSERT INTO pull_dependency_reviews (
    pr_id, repo_id, base_sha, head_sha, conclusion,
    manifest_count, change_count, added_count, removed_count, changed_count,
    vulnerable_change_count, reviewed_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9, $10,
    $11, now()
)
ON CONFLICT (pr_id, base_sha, head_sha) DO UPDATE
SET conclusion = EXCLUDED.conclusion,
    manifest_count = EXCLUDED.manifest_count,
    change_count = EXCLUDED.change_count,
    added_count = EXCLUDED.added_count,
    removed_count = EXCLUDED.removed_count,
    changed_count = EXCLUDED.changed_count,
    vulnerable_change_count = EXCLUDED.vulnerable_change_count,
    reviewed_at = now()
RETURNING *;

-- name: DeletePullDependencyReviewItems :exec
DELETE FROM pull_dependency_review_items
WHERE review_id = $1;

-- name: InsertPullDependencyReviewItem :one
INSERT INTO pull_dependency_review_items (
    review_id, change_kind, ecosystem, package_name, manifest_path,
    lockfile_path, old_version, new_version, scope, direct,
    package_manager, source, advisory_id, severity, advisory_source,
    advisory_external_id, advisory_summary, patched_versions, recommendation
) VALUES (
    sqlc.arg(review_id)::bigint,
    sqlc.arg(change_kind)::text,
    sqlc.arg(ecosystem)::text,
    sqlc.arg(package_name)::text,
    sqlc.arg(manifest_path)::text,
    sqlc.arg(lockfile_path)::text,
    sqlc.arg(old_version)::text,
    sqlc.arg(new_version)::text,
    sqlc.arg(scope)::text,
    sqlc.arg(direct)::boolean,
    sqlc.arg(package_manager)::text,
    sqlc.arg(source)::text,
    sqlc.narg(advisory_id)::bigint,
    sqlc.arg(severity)::text,
    sqlc.arg(advisory_source)::text,
    sqlc.arg(advisory_external_id)::text,
    sqlc.arg(advisory_summary)::text,
    sqlc.arg(patched_versions)::text,
    sqlc.arg(recommendation)::text
)
RETURNING *;

-- name: GetLatestPullDependencyReview :one
SELECT *
FROM pull_dependency_reviews
WHERE pr_id = $1
ORDER BY reviewed_at DESC, id DESC
LIMIT 1;

-- name: GetPullDependencyReviewForHead :one
SELECT *
FROM pull_dependency_reviews
WHERE pr_id = $1
  AND head_sha = $2
ORDER BY reviewed_at DESC, id DESC
LIMIT 1;

-- name: ListPullDependencyReviewItems :many
SELECT *
FROM pull_dependency_review_items
WHERE review_id = $1
ORDER BY
    CASE severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        WHEN 'low' THEN 3
        ELSE 4
    END,
    ecosystem,
    lower(package_name),
    manifest_path,
    change_kind;

-- name: ListDependencyReviewAdvisoryCandidates :many
SELECT
    a.id,
    a.source,
    a.external_id,
    a.ecosystem,
    a.package_name,
    COALESCE(ar.range_expression, a.affected_range) AS affected_range,
    a.patched_versions,
    a.severity,
    a.summary,
    a.description,
    a.reference_urls,
    a.published_at,
    a.withdrawn_at,
    a.created_at,
    a.updated_at,
    a.modified_at,
    a.source_url,
    a.cvss_score,
    a.cvss_vector,
    a.cwe_ids
FROM dependency_advisories a
LEFT JOIN LATERAL (
    SELECT range_expression
    FROM dependency_advisory_affected_ranges ar
    WHERE ar.advisory_id = a.id
      AND lower(ar.ecosystem) = lower(sqlc.arg(ecosystem)::text)
      AND lower(ar.package_name) = lower(sqlc.arg(package_name)::text)
) ar ON true
WHERE a.withdrawn_at IS NULL
  AND (
      ar.range_expression IS NOT NULL
      OR (
          NOT EXISTS (
              SELECT 1
              FROM dependency_advisory_affected_ranges ar2
              WHERE ar2.advisory_id = a.id
          )
          AND lower(a.ecosystem) = lower(sqlc.arg(ecosystem)::text)
          AND lower(a.package_name) = lower(sqlc.arg(package_name)::text)
      )
  )
ORDER BY
    CASE a.severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        ELSE 3
    END,
    id;
