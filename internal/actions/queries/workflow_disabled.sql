-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: IsWorkflowDisabled :one
-- Hot path for trigger.Enqueue: skip enqueueing when the row exists.
SELECT EXISTS (
    SELECT 1 FROM workflow_disabled
    WHERE repo_id = $1 AND workflow_file = $2
) AS disabled;

-- name: ListDisabledWorkflowsForRepo :many
-- Used by the workflows-list endpoint to mark `state: "disabled"`
-- entries without round-tripping through Is for every file.
SELECT workflow_file
FROM workflow_disabled
WHERE repo_id = $1
ORDER BY workflow_file;

-- name: DisableWorkflow :exec
-- Idempotent: re-disabling an already-disabled workflow is a no-op
-- and does not bump disabled_at.
INSERT INTO workflow_disabled (repo_id, workflow_file, disabled_by_user_id)
VALUES ($1, $2, sqlc.narg(disabled_by_user_id)::bigint)
ON CONFLICT (repo_id, workflow_file) DO NOTHING;

-- name: EnableWorkflow :execrows
-- Returns affected-rows so the handler can distinguish 200 (re-enabled)
-- from 404-ish no-op.
DELETE FROM workflow_disabled
WHERE repo_id = $1 AND workflow_file = $2;
