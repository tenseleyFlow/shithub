-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP26b: push-protection bypass request queries.

-- name: UpsertSecretScanBypassRequest :one
-- Hook-side idempotency: repeated rejected pushes for the same exact
-- finding refresh last_seen_at without creating duplicates. Expired
-- approvals revert to pending so the next push requires a fresh review.
INSERT INTO secret_scan_bypass_requests (
    repo_id, pattern, path, commit_oid, line_no, requested_by, request_reason
)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (repo_id, pattern, path, commit_oid, line_no) DO UPDATE
SET last_seen_at = now(),
    requested_by = COALESCE(secret_scan_bypass_requests.requested_by, EXCLUDED.requested_by),
    request_reason = CASE
        WHEN secret_scan_bypass_requests.request_reason = '' THEN EXCLUDED.request_reason
        ELSE secret_scan_bypass_requests.request_reason
    END,
    status = CASE
        WHEN secret_scan_bypass_requests.status = 'approved'
             AND secret_scan_bypass_requests.approved_until <= now()
        THEN 'pending'::secret_scan_bypass_status
        ELSE secret_scan_bypass_requests.status
    END,
    reviewed_by = CASE
        WHEN secret_scan_bypass_requests.status = 'approved'
             AND secret_scan_bypass_requests.approved_until <= now()
        THEN NULL
        ELSE secret_scan_bypass_requests.reviewed_by
    END,
    reviewed_at = CASE
        WHEN secret_scan_bypass_requests.status = 'approved'
             AND secret_scan_bypass_requests.approved_until <= now()
        THEN NULL
        ELSE secret_scan_bypass_requests.reviewed_at
    END,
    review_note = CASE
        WHEN secret_scan_bypass_requests.status = 'approved'
             AND secret_scan_bypass_requests.approved_until <= now()
        THEN ''
        ELSE secret_scan_bypass_requests.review_note
    END,
    approved_until = CASE
        WHEN secret_scan_bypass_requests.status = 'approved'
             AND secret_scan_bypass_requests.approved_until <= now()
        THEN NULL
        ELSE secret_scan_bypass_requests.approved_until
    END
RETURNING *;

-- name: ListSecretScanBypassRequestsForRepo :many
SELECT *
FROM secret_scan_bypass_requests
WHERE repo_id = $1
ORDER BY
    CASE status
        WHEN 'pending' THEN 0
        WHEN 'approved' THEN 1
        ELSE 2
    END,
    created_at DESC,
    id DESC;

-- name: ListApprovedSecretScanBypassesForRepo :many
SELECT *
FROM secret_scan_bypass_requests
WHERE repo_id = $1
  AND status = 'approved'
  AND approved_until > now()
ORDER BY approved_until DESC, id DESC;

-- name: GetSecretScanBypassRequest :one
SELECT *
FROM secret_scan_bypass_requests
WHERE id = $1
  AND repo_id = $2;

-- name: ReviewSecretScanBypassRequest :one
UPDATE secret_scan_bypass_requests
SET status = $3,
    reviewed_by = $4,
    reviewed_at = now(),
    review_note = $5,
    approved_until = CASE
        WHEN $3 = 'approved'::secret_scan_bypass_status THEN $6
        ELSE NULL
    END
WHERE id = $1
  AND repo_id = $2
RETURNING *;
