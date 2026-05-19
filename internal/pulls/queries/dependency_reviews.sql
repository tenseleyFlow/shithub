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
SELECT *
FROM dependency_advisories
WHERE lower(ecosystem) = lower(sqlc.arg(ecosystem)::text)
  AND lower(package_name) = lower(sqlc.arg(package_name)::text)
  AND withdrawn_at IS NULL
ORDER BY
    CASE severity
        WHEN 'critical' THEN 0
        WHEN 'high' THEN 1
        WHEN 'moderate' THEN 2
        ELSE 3
    END,
    id;
