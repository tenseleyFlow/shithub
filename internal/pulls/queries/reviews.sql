-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── pr_reviews ──────────────────────────────────────────────────────

-- name: CreatePRReview :one
INSERT INTO pr_reviews (
    pr_issue_id, author_user_id, state, body, body_html_cached
) VALUES (
    $1, sqlc.narg(author_user_id)::bigint, $2, $3, sqlc.narg(body_html_cached)::text
)
RETURNING *;

-- name: GetPRReviewByID :one
SELECT * FROM pr_reviews WHERE id = $1;

-- name: ListPRReviews :many
SELECT * FROM pr_reviews
WHERE pr_issue_id = $1
ORDER BY submitted_at;

-- name: DismissPRReview :exec
UPDATE pr_reviews
SET dismissed_at = now(),
    dismissed_by_user_id = sqlc.narg(dismissed_by_user_id)::bigint,
    dismissal_reason = $2
WHERE id = $1;

-- name: CountPRReviewsForGate :one
-- Returns the counts the merge gate cares about. `approves` excludes
-- dismissed reviews; `request_changes` includes only undismissed and
-- unsuperseded-by-same-author reviews. The "unsuperseded" semantics
-- (a later review by the same author wins) is computed in Go on the
-- caller side; this query only returns the raw rows it needs.
SELECT
    count(*) FILTER (WHERE state = 'approve' AND dismissed_at IS NULL)::int AS approves,
    count(*) FILTER (WHERE state = 'request_changes' AND dismissed_at IS NULL)::int AS request_changes
FROM pr_reviews
WHERE pr_issue_id = $1;

-- name: ListUndismissedReviewsForGate :many
-- Used by the merge gate to compute "latest review per author" semantics
-- in Go. Ordered author + submitted_at so the caller can pick the last
-- per author cheaply.
SELECT id, pr_issue_id, author_user_id, state, submitted_at
FROM pr_reviews
WHERE pr_issue_id = $1
  AND dismissed_at IS NULL
ORDER BY author_user_id, submitted_at;


-- ─── pr_review_comments ──────────────────────────────────────────────

-- name: CreatePRReviewComment :one
INSERT INTO pr_review_comments (
    pr_issue_id, review_id, author_user_id, file_path, side,
    original_commit_sha, original_line, original_position, current_position,
    body, body_html_cached, in_reply_to_id, pending
) VALUES (
    $1, sqlc.narg(review_id)::bigint, sqlc.narg(author_user_id)::bigint, $2, $3,
    $4, $5, $6, $7,
    $8, sqlc.narg(body_html_cached)::text, sqlc.narg(in_reply_to_id)::bigint, $9
)
RETURNING *;

-- name: GetPRReviewComment :one
SELECT * FROM pr_review_comments WHERE id = $1;

-- name: ListPRReviewComments :many
SELECT * FROM pr_review_comments
WHERE pr_issue_id = $1
ORDER BY created_at;

-- name: ListPRReviewCommentsForFile :many
-- Files-tab fetch: comments anchored to a single file path, oldest first.
SELECT * FROM pr_review_comments
WHERE pr_issue_id = $1
  AND file_path = $2
  AND pending = false
ORDER BY created_at;

-- name: ListPendingDraftCommentsForUser :many
-- Server-side draft listing: rows with pending=true belonging to one
-- user. Ordered by creation so the submit step processes them in order.
SELECT * FROM pr_review_comments
WHERE pr_issue_id = $1
  AND author_user_id = $2
  AND pending = true
ORDER BY created_at;

-- name: AttachPendingCommentsToReview :exec
-- One-shot UPDATE that flips a user's pending draft comments on a PR
-- into the just-submitted review. Runs inside the submit-review tx.
UPDATE pr_review_comments
SET review_id = $3, pending = false, updated_at = now()
WHERE pr_issue_id = $1
  AND author_user_id = $2
  AND pending = true;

-- name: UpdatePRReviewCommentBody :exec
UPDATE pr_review_comments
SET body = $2, body_html_cached = sqlc.narg(body_html_cached)::text,
    edited_at = now(), updated_at = now()
WHERE id = $1;

-- name: SetPRReviewCommentResolved :exec
UPDATE pr_review_comments
SET resolved_at = sqlc.narg(resolved_at)::timestamptz,
    resolved_by_user_id = sqlc.narg(resolved_by_user_id)::bigint,
    updated_at = now()
WHERE id = $1;

-- name: SetPRReviewCommentCurrentPosition :exec
-- Position-mapping update emitted by pulls.Synchronize. NULL means
-- the comment has gone outdated.
UPDATE pr_review_comments
SET current_position = sqlc.narg(current_position)::int,
    updated_at = now()
WHERE id = $1;

-- name: ListNonDraftCommentsForPositionMap :many
-- Position-mapping reads only submitted comments (drafts re-anchor
-- when the user resumes the diff view).
SELECT id, file_path, side, original_commit_sha, original_line, original_position
FROM pr_review_comments
WHERE pr_issue_id = $1
  AND pending = false;


-- ─── pr_review_requests ──────────────────────────────────────────────

-- name: CreatePRReviewRequest :one
INSERT INTO pr_review_requests (
    pr_issue_id, requested_user_id, requested_team_id, requested_by_user_id
) VALUES (
    $1, sqlc.narg(requested_user_id)::bigint, sqlc.narg(requested_team_id)::bigint, sqlc.narg(requested_by_user_id)::bigint
)
RETURNING *;

-- name: ListPRReviewRequests :many
SELECT * FROM pr_review_requests
WHERE pr_issue_id = $1
ORDER BY requested_at;

-- name: ListPRReviewRequestTargets :many
SELECT pr.id, pr.pr_issue_id, pr.requested_user_id, pr.requested_team_id,
       pr.requested_by_user_id, pr.requested_at, pr.dismissed_at,
       pr.satisfied_by_review_id,
       u.username AS requested_username,
       t.slug AS requested_team_slug,
       o.slug AS requested_team_org_slug
FROM pr_review_requests pr
LEFT JOIN users u ON u.id = pr.requested_user_id
LEFT JOIN teams t ON t.id = pr.requested_team_id
LEFT JOIN orgs o ON o.id = t.org_id
WHERE pr.pr_issue_id = $1
ORDER BY pr.requested_at;

-- name: ListPendingReviewRequestsForUser :many
-- Reviewer's inbox feed. Excludes dismissed + satisfied requests.
SELECT pr.* FROM pr_review_requests pr
WHERE requested_user_id = $1
  AND dismissed_at IS NULL
  AND satisfied_by_review_id IS NULL
ORDER BY requested_at DESC;

-- name: SatisfyPRReviewRequest :exec
UPDATE pr_review_requests
SET satisfied_by_review_id = $2
WHERE pr_issue_id = $1
  AND requested_user_id = $3
  AND dismissed_at IS NULL
  AND satisfied_by_review_id IS NULL;

-- name: SatisfyPRReviewTeamRequestsForReviewer :exec
UPDATE pr_review_requests pr
SET satisfied_by_review_id = $2
WHERE pr.pr_issue_id = $1
  AND pr.requested_team_id IS NOT NULL
  AND pr.dismissed_at IS NULL
  AND pr.satisfied_by_review_id IS NULL
  AND EXISTS (
    SELECT 1
    FROM team_members tm
    WHERE tm.team_id = pr.requested_team_id
      AND tm.user_id = $3
  );

-- name: DismissPRReviewRequest :exec
UPDATE pr_review_requests
SET dismissed_at = now()
WHERE id = $1;

-- name: CountActivePRReviewRequests :one
-- Used by the rate-limit gate (max 20 reviewers per PR per the spec
-- pitfall section).
SELECT count(*)::int FROM pr_review_requests
WHERE pr_issue_id = $1
  AND dismissed_at IS NULL;
