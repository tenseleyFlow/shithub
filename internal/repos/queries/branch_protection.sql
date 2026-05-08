-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: ListBranchProtectionRules :many
SELECT id, repo_id, pattern,
       prevent_force_push, prevent_deletion, require_pr_for_push,
       allowed_pusher_user_ids,
       require_signed_commits, status_checks_required,
       created_at, updated_at, created_by_user_id,
       required_review_count, dismiss_stale_reviews_on_push, require_code_owner_review,
       dismiss_stale_status_checks_on_push
FROM branch_protection_rules
WHERE repo_id = $1
ORDER BY pattern;

-- name: GetBranchProtectionRule :one
SELECT id, repo_id, pattern,
       prevent_force_push, prevent_deletion, require_pr_for_push,
       allowed_pusher_user_ids,
       require_signed_commits, status_checks_required,
       created_at, updated_at, created_by_user_id,
       required_review_count, dismiss_stale_reviews_on_push, require_code_owner_review,
       dismiss_stale_status_checks_on_push
FROM branch_protection_rules
WHERE id = $1;

-- name: UpsertBranchProtectionRule :one
INSERT INTO branch_protection_rules (
    repo_id, pattern,
    prevent_force_push, prevent_deletion, require_pr_for_push,
    allowed_pusher_user_ids, created_by_user_id
) VALUES (
    $1, $2, $3, $4, $5, $6, sqlc.narg(created_by_user_id)::bigint
)
RETURNING id;

-- name: UpdateBranchProtectionRule :exec
UPDATE branch_protection_rules
SET pattern = $2,
    prevent_force_push = $3,
    prevent_deletion = $4,
    require_pr_for_push = $5,
    allowed_pusher_user_ids = $6
WHERE id = $1;

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
