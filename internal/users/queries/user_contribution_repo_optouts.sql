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

-- name: ReplaceContributionOptoutsForUser :exec
-- Atomic reconcile: insert any desired repo IDs that aren't already
-- opt-outs, and delete any existing opt-outs not in the desired set.
-- Both data-modifying CTEs see the same snapshot, so we don't need an
-- explicit tx wrapper. Retires the per-row upsert+delete loop from the
-- pre-PRO-EXT_SR2-12 settings/contributions submit handler.
WITH desired AS (
    SELECT unnest(@repo_ids::bigint[]) AS repo_id
),
ins AS (
    INSERT INTO user_contribution_repo_optouts (user_id, repo_id)
    SELECT @user_id::bigint, repo_id FROM desired
    ON CONFLICT (user_id, repo_id) DO NOTHING
)
DELETE FROM user_contribution_repo_optouts
WHERE user_id = @user_id::bigint
  AND repo_id NOT IN (SELECT repo_id FROM desired);
