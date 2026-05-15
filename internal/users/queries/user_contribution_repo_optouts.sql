-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-09: per-repo contribution-graph opt-outs.

-- name: ListContributionOptoutsForUser :many
SELECT user_id, repo_id, created_at
FROM user_contribution_repo_optouts
WHERE user_id = $1;

-- name: ListContributionOptoutRepoIDsForUser :many
SELECT repo_id
FROM user_contribution_repo_optouts
WHERE user_id = $1;

-- name: UpsertContributionOptout :exec
-- Idempotent on (user_id, repo_id); a duplicate insert is a no-op.
INSERT INTO user_contribution_repo_optouts (user_id, repo_id)
VALUES ($1, $2)
ON CONFLICT (user_id, repo_id) DO NOTHING;

-- name: DeleteContributionOptout :exec
DELETE FROM user_contribution_repo_optouts
WHERE user_id = $1 AND repo_id = $2;
