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

-- H15 / H14: gate on the state actually changing. Pre-fix
-- `setState` unconditionally rewrote state_reason + closed_at even
-- when the state value was already the requested one — so two
-- concurrent close-with-comment calls each succeeded AND emitted a
-- "closed" timeline event AND posted a comment, persisting both. The
-- `WHERE state != $2` clause makes the UPDATE a no-op for already-
-- closed-and-now-closed (and same for open), and the :execrows
-- variant lets the caller observe whether a transition actually
-- happened so it can gate downstream side-effects (event emit, audit
-- record, CLI's success line).
-- name: SetIssueState :execrows
UPDATE issues
SET state = $2,
    state_reason = sqlc.narg(state_reason)::issue_state_reason,
    closed_at = CASE WHEN $2::issue_state = 'closed' THEN now() ELSE NULL END,
    closed_by_user_id = sqlc.narg(closed_by_user_id)::bigint,
    updated_at = now()
WHERE id = $1 AND state <> $2;

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

-- name: DeleteIssue :exec
-- G8 (F45): hard-delete an issue row. The `issues` table has every
-- child relation wired ON DELETE CASCADE (comments, labels, assignees,
-- milestones, events, references, search index, …), so the cascade
-- does the rest. Caller is responsible for the policy gate
-- (ActionRepoAdmin) and audit emission.
DELETE FROM issues WHERE id = $1 AND kind = 'issue';


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

-- name: CountIssueAssignees :one
SELECT count(*) FROM issue_assignees WHERE issue_id = $1;

-- name: IssueAssigneeExists :one
SELECT EXISTS (
    SELECT 1
    FROM issue_assignees
    WHERE issue_id = $1 AND user_id = $2
);


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

-- name: ListIssueEventsWithActor :many
-- Paginated timeline shape for the REST `/issues/{n}/events` endpoint:
-- the same event rows ListIssueEvents returns, but LEFT-joined to users
-- so the response can carry `actor_username` without a second round-trip.
-- Suspended/deleted actor rows still appear (the timeline is historical
-- truth), with NULL username when the user row is unrecoverable.
SELECT e.id, e.issue_id, e.actor_user_id, e.kind, e.meta, e.ref_target_id,
       e.created_at, u.username AS actor_username
FROM issue_events e
LEFT JOIN users u ON u.id = e.actor_user_id
WHERE e.issue_id = $1
ORDER BY e.created_at ASC, e.id ASC
LIMIT $2 OFFSET $3;

-- name: CountIssueEvents :one
SELECT COUNT(*) FROM issue_events WHERE issue_id = $1;

-- name: ListProfileAuthoredIssuesForUser :many
-- Cross-repository profile contribution activity. The handler performs the
-- final repo visibility gate with policy.IsVisibleTo so private issues and
-- PRs never leak through the public profile timeline.
SELECT
    i.id, i.repo_id, i.number, i.kind, i.title, i.state, i.created_at, i.closed_at,
    pr.merged_at,
    r.name AS repo_name, r.visibility, r.owner_user_id, r.owner_org_id,
    COALESCE(u.username, o.slug)::text AS owner_slug
FROM issues i
JOIN repos r ON r.id = i.repo_id
LEFT JOIN pull_requests pr ON pr.issue_id = i.id
LEFT JOIN users u ON u.id = r.owner_user_id
LEFT JOIN orgs o ON o.id = r.owner_org_id
WHERE i.author_user_id = $1
  AND i.created_at >= $2
  AND i.created_at < $3
  AND r.deleted_at IS NULL
ORDER BY i.created_at DESC, i.id DESC
LIMIT $4;

-- name: InsertIssueReference :exec
INSERT INTO issue_references (
    source_issue_id, target_issue_id, source_kind, source_object_id
) VALUES (
    sqlc.narg(source_issue_id)::bigint, $1, $2, sqlc.narg(source_object_id)::bigint
);
