-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertWorkflowRunApproval :one
INSERT INTO workflow_run_approvals (run_id, requested_reason)
VALUES ($1, $2)
ON CONFLICT (run_id) DO UPDATE SET
    requested_reason = EXCLUDED.requested_reason,
    updated_at = now()
RETURNING run_id, requested_reason, requested_at,
          approved_by_user_id, approved_at,
          rejected_by_user_id, rejected_at,
          created_at, updated_at;

-- name: GetWorkflowRunApproval :one
SELECT run_id, requested_reason, requested_at,
       approved_by_user_id, approved_at,
       rejected_by_user_id, rejected_at,
       created_at, updated_at
FROM workflow_run_approvals
WHERE run_id = $1;

-- name: ApproveWorkflowRun :one
WITH run AS (
    UPDATE workflow_runs r
    SET approved_by_user_id = $2,
        version = version + 1,
        updated_at = now()
    WHERE r.id = $1
      AND r.need_approval = true
      AND r.approved_by_user_id IS NULL
      AND r.status = 'queued'
      AND EXISTS (
          SELECT 1
          FROM workflow_run_approvals a
          WHERE a.run_id = r.id
            AND a.approved_at IS NULL
            AND a.rejected_at IS NULL
      )
    RETURNING r.id, r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
              r.head_sha, r.head_ref, r.event, r.event_payload,
              r.actor_user_id, r.parent_run_id, r.concurrency_group,
              r.status, r.conclusion, r.pinned, r.need_approval, r.approved_by_user_id,
              r.started_at, r.completed_at, r.version, r.created_at, r.updated_at, r.trigger_event_id
), approval AS (
    UPDATE workflow_run_approvals a
    SET approved_by_user_id = $2,
        approved_at = now(),
        updated_at = now()
    FROM run r
    WHERE a.run_id = r.id
      AND a.approved_at IS NULL
      AND a.rejected_at IS NULL
    RETURNING a.run_id
)
SELECT r.id, r.repo_id, r.run_index, r.workflow_file, r.workflow_name,
       r.head_sha, r.head_ref, r.event, r.event_payload,
       r.actor_user_id, r.parent_run_id, r.concurrency_group,
       r.status, r.conclusion, r.pinned, r.need_approval, r.approved_by_user_id,
       r.started_at, r.completed_at, r.version, r.created_at, r.updated_at, r.trigger_event_id
FROM run r
JOIN approval a ON a.run_id = r.id;

-- name: RejectWorkflowRunApproval :one
UPDATE workflow_run_approvals
SET rejected_by_user_id = $2,
    rejected_at = now(),
    updated_at = now()
WHERE run_id = $1
  AND approved_at IS NULL
  AND rejected_at IS NULL
RETURNING run_id, requested_reason, requested_at,
          approved_by_user_id, approved_at,
          rejected_by_user_id, rejected_at,
          created_at, updated_at;

-- name: MarkWorkflowRunRejected :one
UPDATE workflow_runs
SET status = 'completed',
    conclusion = 'action_required',
    started_at = COALESCE(started_at, now()),
    completed_at = COALESCE(completed_at, now()),
    version = version + 1,
    updated_at = now()
WHERE id = $1
  AND need_approval = true
  AND approved_by_user_id IS NULL
  AND status = 'queued'
RETURNING id, repo_id, run_index, workflow_file, workflow_name,
          head_sha, head_ref, event, event_payload,
          actor_user_id, parent_run_id, concurrency_group,
          status, conclusion, pinned, need_approval, approved_by_user_id,
          started_at, completed_at, version, created_at, updated_at, trigger_event_id;

-- name: MarkWorkflowJobsRejected :many
UPDATE workflow_jobs
SET cancel_requested = true,
    status = 'cancelled',
    conclusion = 'action_required',
    started_at = COALESCE(started_at, now()),
    completed_at = COALESCE(completed_at, now()),
    version = version + 1,
    updated_at = now()
WHERE run_id = $1
  AND status = 'queued'
RETURNING id, run_id, job_index, job_key, job_name, runs_on,
          runner_id, needs_jobs, if_expr, timeout_minutes, permissions,
          job_env, status, conclusion, cancel_requested,
          started_at, completed_at, version, created_at, updated_at;
