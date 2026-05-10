-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── check_suites ────────────────────────────────────────────────────

-- name: GetOrCreateCheckSuite :one
-- Idempotent suite-by-(repo, head_sha, app_slug) lookup. Used by every
-- check-run create so consumers don't need to manage suite ids. ON
-- CONFLICT … DO UPDATE returns the existing row when the unique
-- (repo_id, head_sha, app_slug) already exists; otherwise returns the
-- freshly inserted one.
INSERT INTO check_suites (repo_id, head_sha, app_slug)
VALUES ($1, $2, $3)
ON CONFLICT (repo_id, head_sha, app_slug) DO UPDATE
    SET app_slug = EXCLUDED.app_slug
RETURNING *;

-- name: GetCheckSuite :one
SELECT * FROM check_suites WHERE id = $1;

-- name: GetCheckSuiteForRepo :one
SELECT
    cs.*,
    COALESCE(pr_meta.number, 0)::bigint AS pull_number,
    COALESCE(pr_meta.title, '')::text AS pull_title,
    COALESCE(pr_meta.author_username, '')::text AS pull_author_username,
    COALESCE(pr_meta.head_ref, '')::text AS head_ref,
    COALESCE(pr_meta.base_ref, '')::text AS base_ref
FROM check_suites cs
LEFT JOIN LATERAL (
    SELECT
        i.number,
        i.title,
        COALESCE(u.username, '') AS author_username,
        pr.head_ref,
        pr.base_ref
    FROM pull_requests pr
    JOIN issues i ON i.id = pr.issue_id AND i.kind = 'pr'
    LEFT JOIN users u ON u.id = i.author_user_id
    WHERE pr.head_repo_id = cs.repo_id
      AND pr.head_oid = cs.head_sha
    ORDER BY i.updated_at DESC, i.number DESC
    LIMIT 1
) pr_meta ON true
WHERE cs.repo_id = $1 AND cs.id = $2;

-- name: ListCheckSuitesForRepo :many
SELECT
    cs.*,
    COALESCE(pr_meta.number, 0)::bigint AS pull_number,
    COALESCE(pr_meta.title, '')::text AS pull_title,
    COALESCE(pr_meta.author_username, '')::text AS pull_author_username,
    COALESCE(pr_meta.head_ref, '')::text AS head_ref,
    COALESCE(pr_meta.base_ref, '')::text AS base_ref
FROM check_suites cs
LEFT JOIN LATERAL (
    SELECT
        i.number,
        i.title,
        COALESCE(u.username, '') AS author_username,
        pr.head_ref,
        pr.base_ref
    FROM pull_requests pr
    JOIN issues i ON i.id = pr.issue_id AND i.kind = 'pr'
    LEFT JOIN users u ON u.id = i.author_user_id
    WHERE pr.head_repo_id = cs.repo_id
      AND pr.head_oid = cs.head_sha
    ORDER BY i.updated_at DESC, i.number DESC
    LIMIT 1
) pr_meta ON true
WHERE cs.repo_id = $1
ORDER BY cs.updated_at DESC, cs.id DESC
LIMIT $2 OFFSET $3;

-- name: ListCheckSuitesForCommit :many
SELECT * FROM check_suites
WHERE repo_id = $1 AND head_sha = $2
ORDER BY app_slug;

-- name: UpdateCheckSuiteRollup :exec
-- Persists the rollup result computed in Go (suite_rollup.go).
UPDATE check_suites
SET status = $2,
    conclusion = sqlc.narg(conclusion)::check_conclusion
WHERE id = $1;

-- name: ListCheckSuiteIDsForHead :many
-- Used by the stale-on-push hook to flip queued/in_progress suites on
-- the previous head to conclusion='stale'.
SELECT id FROM check_suites
WHERE repo_id = $1 AND head_sha = $2 AND status <> 'completed';

-- name: MarkCheckSuiteStale :exec
UPDATE check_suites
SET status = 'completed', conclusion = 'stale'
WHERE id = $1;


-- ─── check_runs ──────────────────────────────────────────────────────

-- name: GetCheckRunByExternalID :one
-- External-system create dedupe: lookup by (repo, head_sha, name,
-- external_id). NULL external_id never matches via this query.
SELECT * FROM check_runs
WHERE repo_id = $1 AND head_sha = $2 AND name = $3 AND external_id = $4;

-- name: CreateCheckRun :one
INSERT INTO check_runs (
    suite_id, repo_id, head_sha, name,
    status, conclusion,
    started_at, completed_at,
    details_url, output, external_id
) VALUES (
    $1, $2, $3, $4,
    $5, sqlc.narg(conclusion)::check_conclusion,
    sqlc.narg(started_at)::timestamptz, sqlc.narg(completed_at)::timestamptz,
    $6, $7, sqlc.narg(external_id)::text
)
RETURNING *;

-- name: GetCheckRun :one
SELECT * FROM check_runs WHERE id = $1;

-- name: UpdateCheckRun :exec
UPDATE check_runs
SET status = $2,
    conclusion = sqlc.narg(conclusion)::check_conclusion,
    started_at = sqlc.narg(started_at)::timestamptz,
    completed_at = sqlc.narg(completed_at)::timestamptz,
    details_url = $3,
    output = $4
WHERE id = $1;

-- name: ListCheckRunsForCommit :many
SELECT * FROM check_runs
WHERE repo_id = $1 AND head_sha = $2
ORDER BY name;

-- name: ListCheckRunsBySuite :many
SELECT * FROM check_runs
WHERE suite_id = $1
ORDER BY name;

-- name: GetLatestCheckRunByName :one
-- Required-check evaluator: most recent run with the given name on the
-- specified head_sha.
SELECT * FROM check_runs
WHERE repo_id = $1 AND head_sha = $2 AND name = $3
ORDER BY created_at DESC
LIMIT 1;
