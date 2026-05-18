-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── pull_requests core ──────────────────────────────────────────────

-- name: CreatePullRequest :one
-- Creates the PR-side row keyed on issue_id (the caller already
-- inserted the issues row with kind='pr'). Defaults give a freshly-
-- opened PR `mergeable_state='unknown'` until the mergeability job
-- ticks.
INSERT INTO pull_requests (
    issue_id, base_ref, head_ref, head_repo_id,
    base_oid, head_oid, draft
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
RETURNING *;

-- name: GetPullRequestByIssueID :one
SELECT * FROM pull_requests WHERE issue_id = $1;

-- name: GetPullRequestByRepoAndNumber :one
-- Joins issues + pull_requests so handlers can resolve via the URL
-- {owner}/{repo}/pulls/{number} in one round-trip.
SELECT
    pr.issue_id, pr.base_ref, pr.head_ref, pr.head_repo_id,
    pr.base_oid, pr.head_oid, pr.draft, pr.mergeable, pr.mergeable_state,
    pr.merge_commit_sha, pr.merged_at, pr.merged_by_user_id, pr.merge_method,
    pr.base_oid_at_merge, pr.head_oid_at_merge, pr.last_synchronized_at,
    i.id              AS i_id,
    i.repo_id         AS i_repo_id,
    i.number          AS i_number,
    i.title           AS i_title,
    i.body            AS i_body,
    i.body_html_cached AS i_body_html_cached,
    i.author_user_id  AS i_author_user_id,
    i.state           AS i_state,
    i.locked          AS i_locked,
    i.created_at      AS i_created_at,
    i.updated_at      AS i_updated_at
FROM pull_requests pr
JOIN issues i ON i.id = pr.issue_id
WHERE i.repo_id = $1 AND i.number = $2 AND i.kind = 'pr';

-- name: ListPullRequestsByRepo :many
-- Mirrors the issues list query: state filter via narg, pagination,
-- ordered by recent activity.
SELECT pr.issue_id, pr.base_ref, pr.head_ref, pr.head_repo_id,
       pr.base_oid, pr.head_oid, pr.draft, pr.mergeable, pr.mergeable_state,
       pr.merge_commit_sha, pr.merged_at, pr.merged_by_user_id, pr.merge_method,
       pr.base_oid_at_merge, pr.head_oid_at_merge, pr.last_synchronized_at,
       i.id, i.repo_id, i.number, i.title, i.body, i.author_user_id,
       i.state, i.created_at, i.updated_at
FROM pull_requests pr
JOIN issues i ON i.id = pr.issue_id
WHERE i.repo_id = $1
  AND i.kind = 'pr'
  AND (sqlc.narg(state_filter)::text IS NULL OR i.state::text = sqlc.narg(state_filter)::text)
  AND (sqlc.narg(draft)::boolean IS NULL OR pr.draft = sqlc.narg(draft)::boolean)
ORDER BY i.updated_at DESC
LIMIT $2 OFFSET $3;

-- name: CountPullRequestsByRepo :one
SELECT count(*)::bigint FROM pull_requests pr
JOIN issues i ON i.id = pr.issue_id
WHERE i.repo_id = $1
  AND i.kind = 'pr'
  AND (sqlc.narg(state_filter)::text IS NULL OR i.state::text = sqlc.narg(state_filter)::text)
  AND (sqlc.narg(draft)::boolean IS NULL OR pr.draft = sqlc.narg(draft)::boolean);

-- name: SetPullRequestSnapshot :exec
-- Updates base_oid + head_oid + last_synchronized_at after a
-- synchronize tick.
UPDATE pull_requests
SET base_oid = $2,
    head_oid = $3,
    last_synchronized_at = now()
WHERE issue_id = $1;

-- name: SetPullRequestMergeability :exec
UPDATE pull_requests
SET mergeable = sqlc.narg(mergeable)::boolean,
    mergeable_state = $2
WHERE issue_id = $1;

-- name: SetPullRequestDraft :exec
UPDATE pull_requests
SET draft = $2
WHERE issue_id = $1;

-- name: LockPullRequestForMerge :one
-- FOR UPDATE row lock + return current shape so the merge job can
-- decide whether to proceed (e.g. someone else just merged it).
SELECT * FROM pull_requests
WHERE issue_id = $1
FOR UPDATE;

-- name: SetPullRequestMerged :exec
UPDATE pull_requests
SET merged_at = now(),
    merged_by_user_id = sqlc.narg(merged_by_user_id)::bigint,
    merge_commit_sha = $2,
    merge_method = $3,
    base_oid_at_merge = $4,
    head_oid_at_merge = $5,
    mergeable = false,
    mergeable_state = 'clean'
WHERE issue_id = $1;


-- ─── commits + files (refreshed on synchronize) ──────────────────────

-- name: ClearPullRequestCommits :exec
DELETE FROM pull_request_commits WHERE pr_id = $1;

-- name: InsertPullRequestCommit :exec
INSERT INTO pull_request_commits (
    pr_id, sha, position, author_name, author_email,
    committer_name, committer_email, subject, body,
    authored_at, committed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9,
    sqlc.narg(authored_at)::timestamptz,
    sqlc.narg(committed_at)::timestamptz
);

-- name: ListPullRequestCommits :many
SELECT * FROM pull_request_commits
WHERE pr_id = $1
ORDER BY position;

-- name: ClearPullRequestFiles :exec
DELETE FROM pull_request_files WHERE pr_id = $1;

-- name: InsertPullRequestFile :exec
INSERT INTO pull_request_files (
    pr_id, path, status, old_path, additions, deletions, changes
) VALUES (
    $1, $2, $3, sqlc.narg(old_path)::text, $4, $5, $6
);

-- name: ListPullRequestFiles :many
SELECT * FROM pull_request_files
WHERE pr_id = $1
ORDER BY path;


-- name: ListOpenPRsForHeadRef :many
-- Returns the issue_ids of every still-open PR whose head_repo_id +
-- head_ref match the pushed ref. push:process uses this to fan-out
-- pr:synchronize jobs after a head-side push.
SELECT pr.issue_id
FROM pull_requests pr
JOIN issues i ON i.id = pr.issue_id
WHERE pr.head_repo_id = $1
  AND pr.head_ref = $2
  AND i.state = 'open'
  AND pr.merged_at IS NULL;


-- name: ListOpenPRsForHeadSHA :many
-- Returns the issue_ids of every still-open PR whose head_repo_id +
-- head_oid match a given SHA. Used by the check-completion trigger
-- (S64) to fan-out pr:mergeability jobs once CI for a head SHA
-- finishes — the required-checks gate inside Mergeability needs a
-- recompute to flip blocked → clean.
SELECT pr.issue_id
FROM pull_requests pr
JOIN issues i ON i.id = pr.issue_id
WHERE pr.head_repo_id = $1
  AND pr.head_oid = $2
  AND i.state = 'open'
  AND pr.merged_at IS NULL;
