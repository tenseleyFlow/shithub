-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: GetRepoInsightSnapshot :one
SELECT repo_id, default_branch, head_sha, captured_at, commit_count,
       contributor_count, additions, deletions, data
FROM repo_insight_snapshots
WHERE repo_id = $1;

-- name: UpsertRepoInsightSnapshot :one
INSERT INTO repo_insight_snapshots (
    repo_id, default_branch, head_sha, captured_at, commit_count,
    contributor_count, additions, deletions, data
) VALUES (
    $1, $2, $3, now(), $4, $5, $6, $7, sqlc.arg(data)::jsonb
)
ON CONFLICT (repo_id) DO UPDATE SET
    default_branch = EXCLUDED.default_branch,
    head_sha = EXCLUDED.head_sha,
    captured_at = EXCLUDED.captured_at,
    commit_count = EXCLUDED.commit_count,
    contributor_count = EXCLUDED.contributor_count,
    additions = EXCLUDED.additions,
    deletions = EXCLUDED.deletions,
    data = EXCLUDED.data
RETURNING repo_id, default_branch, head_sha, captured_at, commit_count,
          contributor_count, additions, deletions, data;

-- name: DeleteRepoInsightSnapshot :exec
DELETE FROM repo_insight_snapshots
WHERE repo_id = $1;
