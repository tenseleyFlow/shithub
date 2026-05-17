-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: ListBranchProtectionRules :many
SELECT id, repo_id, pattern,
       prevent_force_push, prevent_deletion, require_pr_for_push,
       allowed_pusher_user_ids,
       require_signed_commits, status_checks_required,
       created_at, updated_at, created_by_user_id,
       required_review_count, dismiss_stale_reviews_on_push, require_code_owner_review,
       dismiss_stale_status_checks_on_push,
       target
FROM branch_protection_rules
WHERE repo_id = $1
ORDER BY target, pattern;

-- name: GetBranchProtectionRule :one
SELECT id, repo_id, pattern,
       prevent_force_push, prevent_deletion, require_pr_for_push,
       allowed_pusher_user_ids,
       require_signed_commits, status_checks_required,
       created_at, updated_at, created_by_user_id,
       required_review_count, dismiss_stale_reviews_on_push, require_code_owner_review,
       dismiss_stale_status_checks_on_push,
       target
FROM branch_protection_rules
WHERE id = $1;

-- name: UpsertBranchProtectionRule :one
INSERT INTO branch_protection_rules (
    repo_id, pattern, target,
    prevent_force_push, prevent_deletion, require_pr_for_push,
    require_signed_commits,
    allowed_pusher_user_ids, created_by_user_id
) VALUES (
    sqlc.arg(repo_id)::bigint,
    sqlc.arg(pattern)::text,
    COALESCE(NULLIF(sqlc.arg(target)::text, ''), 'branch'),
    sqlc.arg(prevent_force_push)::boolean,
    sqlc.arg(prevent_deletion)::boolean,
    sqlc.arg(require_pr_for_push)::boolean,
    sqlc.arg(require_signed_commits)::boolean,
    sqlc.arg(allowed_pusher_user_ids)::bigint[],
    sqlc.narg(created_by_user_id)::bigint
)
RETURNING id;

-- name: UpdateBranchProtectionRule :exec
UPDATE branch_protection_rules
SET pattern = sqlc.arg(pattern)::text,
    target = COALESCE(NULLIF(sqlc.arg(target)::text, ''), 'branch'),
    prevent_force_push = sqlc.arg(prevent_force_push)::boolean,
    prevent_deletion = sqlc.arg(prevent_deletion)::boolean,
    require_pr_for_push = sqlc.arg(require_pr_for_push)::boolean,
    require_signed_commits = sqlc.arg(require_signed_commits)::boolean,
    allowed_pusher_user_ids = sqlc.arg(allowed_pusher_user_ids)::bigint[]
WHERE id = sqlc.arg(id)::bigint;

-- name: UpdateBranchProtectionReviewSettings :exec
-- S23 surface for the review-related knobs. Branch-protection edit
-- handler calls this alongside UpdateBranchProtectionRule.
UPDATE branch_protection_rules
SET required_review_count = $2,
    dismiss_stale_reviews_on_push = $3,
    require_code_owner_review = $4
WHERE id = $1;

-- name: UpdateBranchProtectionCheckSettings :exec
-- S24 surface for the required-status-check knobs. Branch-protection
-- edit handler calls this alongside UpdateBranchProtectionRule.
UPDATE branch_protection_rules
SET status_checks_required = $2,
    dismiss_stale_status_checks_on_push = $3
WHERE id = $1;

-- name: DeleteBranchProtectionRule :exec
DELETE FROM branch_protection_rules WHERE id = $1;

-- name: UpdateRepoDefaultBranch :exec
-- Used by the default-branch settings handler. The on-disk HEAD update
-- is a separate step done via `git symbolic-ref` from the orchestrator.
UPDATE repos SET default_branch = $2, updated_at = now() WHERE id = $1;
