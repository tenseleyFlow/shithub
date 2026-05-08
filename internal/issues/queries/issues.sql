-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── per-repo numbering ───────────────────────────────────────────────

-- name: EnsureRepoIssueCounter :exec
-- Lazy-initialize the counter row. Idempotent — invoked from repo
-- create AND from the first issue insert (defensive in case someone
-- migrates an old repo that predates S21).
INSERT INTO repo_issue_counter (repo_id, next_number)
VALUES ($1, 1)
ON CONFLICT (repo_id) DO NOTHING;

-- name: AllocateIssueNumber :one
-- UPDATE … RETURNING is concurrency-safe: each row update is
-- serialized by the row lock; concurrent transactions see different
-- values. The caller wraps this in the same tx as the issue insert.
UPDATE repo_issue_counter
SET next_number = next_number + 1
WHERE repo_id = $1
RETURNING (next_number - 1)::bigint AS allocated;


-- ─── issues ──────────────────────────────────────────────────────────

-- name: CreateIssue :one
INSERT INTO issues (
    repo_id, number, kind, title, body, author_user_id
) VALUES (
    $1, $2, $3, $4, $5, sqlc.narg(author_user_id)::bigint
)
RETURNING *;

-- name: GetIssueByNumber :one
SELECT * FROM issues
WHERE repo_id = $1 AND number = $2;

-- name: GetIssueByID :one
SELECT * FROM issues WHERE id = $1;

-- name: ListIssues :many
-- Filterable list. Caller passes a state filter (open/closed/all
-- where 'all' is encoded as NULL); label/assignee/author/milestone
-- filtering happens after this query in Go for v1 — see the
-- internal/issues/list.go composer. Per-page hardcoded at 25 in the
-- handler; offset is the (page-1)*25.
SELECT * FROM issues
WHERE repo_id = $1
  AND (sqlc.narg(state_filter)::text IS NULL OR state::text = sqlc.narg(state_filter)::text)
  AND kind = COALESCE(sqlc.narg(kind)::issue_kind, 'issue')
ORDER BY updated_at DESC
LIMIT $2 OFFSET $3;

-- name: CountIssues :one
SELECT count(*)::bigint FROM issues
WHERE repo_id = $1
  AND (sqlc.narg(state_filter)::text IS NULL OR state::text = sqlc.narg(state_filter)::text)
  AND kind = COALESCE(sqlc.narg(kind)::issue_kind, 'issue');

-- name: UpdateIssueTitleBody :exec
UPDATE issues
SET title = $2, body = $3, body_html_cached = $4, edited_at = now(), updated_at = now()
WHERE id = $1;

-- name: SetIssueState :exec
UPDATE issues
SET state = $2,
    state_reason = sqlc.narg(state_reason)::issue_state_reason,
    closed_at = CASE WHEN $2::issue_state = 'closed' THEN now() ELSE NULL END,
    closed_by_user_id = sqlc.narg(closed_by_user_id)::bigint,
    updated_at = now()
WHERE id = $1;

-- name: SetIssueLock :exec
UPDATE issues
SET locked = $2, lock_reason = sqlc.narg(lock_reason)::text, updated_at = now()
WHERE id = $1;

-- name: SetIssueMilestone :exec
UPDATE issues
SET milestone_id = sqlc.narg(milestone_id)::bigint, updated_at = now()
WHERE id = $1;


-- ─── comments ────────────────────────────────────────────────────────

-- name: CreateIssueComment :one
INSERT INTO issue_comments (issue_id, author_user_id, body, body_html_cached)
VALUES ($1, sqlc.narg(author_user_id)::bigint, $2, $3)
RETURNING *;

-- name: ListIssueComments :many
SELECT * FROM issue_comments
WHERE issue_id = $1
ORDER BY created_at ASC;

-- name: GetIssueComment :one
SELECT * FROM issue_comments WHERE id = $1;

-- name: UpdateIssueCommentBody :exec
UPDATE issue_comments
SET body = $2, body_html_cached = $3, edited_at = now(), updated_at = now()
WHERE id = $1;

-- name: DeleteIssueComment :exec
DELETE FROM issue_comments WHERE id = $1;


-- ─── assignees ───────────────────────────────────────────────────────

-- name: AssignUserToIssue :exec
INSERT INTO issue_assignees (issue_id, user_id, assigned_by_user_id)
VALUES ($1, $2, sqlc.narg(assigned_by_user_id)::bigint)
ON CONFLICT (issue_id, user_id) DO NOTHING;

-- name: UnassignUserFromIssue :exec
DELETE FROM issue_assignees WHERE issue_id = $1 AND user_id = $2;

-- name: ListIssueAssignees :many
SELECT a.issue_id, a.user_id, a.assigned_at, u.username, u.display_name
FROM issue_assignees a
JOIN users u ON u.id = a.user_id
WHERE a.issue_id = $1
ORDER BY a.assigned_at;


-- ─── labels ──────────────────────────────────────────────────────────

-- name: CreateLabel :one
INSERT INTO labels (repo_id, name, color, description)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListLabels :many
SELECT * FROM labels WHERE repo_id = $1 ORDER BY name;

-- name: GetLabelByName :one
SELECT * FROM labels WHERE repo_id = $1 AND name = $2;

-- name: UpdateLabel :exec
UPDATE labels
SET name = $2, color = $3, description = $4
WHERE id = $1;

-- name: DeleteLabel :exec
DELETE FROM labels WHERE id = $1;


-- ─── issue ↔ label ───────────────────────────────────────────────────

-- name: AddIssueLabel :exec
INSERT INTO issue_labels (issue_id, label_id, applied_by_user_id)
VALUES ($1, $2, sqlc.narg(applied_by_user_id)::bigint)
ON CONFLICT (issue_id, label_id) DO NOTHING;

-- name: RemoveIssueLabel :exec
DELETE FROM issue_labels WHERE issue_id = $1 AND label_id = $2;

-- name: ListLabelsOnIssue :many
SELECT l.id, l.repo_id, l.name, l.color, l.description, l.created_at
FROM issue_labels il
JOIN labels l ON l.id = il.label_id
WHERE il.issue_id = $1
ORDER BY l.name;


-- ─── milestones ──────────────────────────────────────────────────────

-- name: CreateMilestone :one
INSERT INTO milestones (repo_id, title, description, due_on)
VALUES ($1, $2, $3, sqlc.narg(due_on)::timestamptz)
RETURNING *;

-- name: ListMilestones :many
SELECT * FROM milestones WHERE repo_id = $1 ORDER BY state, due_on NULLS LAST, title;

-- name: GetMilestone :one
SELECT * FROM milestones WHERE id = $1;

-- name: UpdateMilestone :exec
UPDATE milestones SET title = $2, description = $3, due_on = sqlc.narg(due_on)::timestamptz WHERE id = $1;

-- name: SetMilestoneState :exec
UPDATE milestones SET state = $2, closed_at = CASE WHEN $2::milestone_state = 'closed' THEN now() ELSE NULL END WHERE id = $1;

-- name: DeleteMilestone :exec
DELETE FROM milestones WHERE id = $1;

-- name: MilestoneIssueCounts :one
-- Open + closed counts for the milestone progress bar.
SELECT
    count(*) FILTER (WHERE state = 'open')::int   AS open_count,
    count(*) FILTER (WHERE state = 'closed')::int AS closed_count
FROM issues
WHERE milestone_id = $1;


-- ─── events + references ─────────────────────────────────────────────

-- name: InsertIssueEvent :one
INSERT INTO issue_events (issue_id, actor_user_id, kind, meta, ref_target_id)
VALUES ($1, sqlc.narg(actor_user_id)::bigint, $2, $3, sqlc.narg(ref_target_id)::bigint)
RETURNING *;

-- name: ListIssueEvents :many
SELECT * FROM issue_events
WHERE issue_id = $1
ORDER BY created_at ASC;

-- name: InsertIssueReference :exec
INSERT INTO issue_references (
    source_issue_id, target_issue_id, source_kind, source_object_id
) VALUES (
    sqlc.narg(source_issue_id)::bigint, $1, $2, sqlc.narg(source_object_id)::bigint
);
