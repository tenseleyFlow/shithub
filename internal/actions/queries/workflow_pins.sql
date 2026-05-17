-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: ListWorkflowPinsForUserRepo :many
SELECT user_id, repo_id, workflow_file, position, created_at, updated_at
FROM user_action_workflow_pins
WHERE user_id = $1 AND repo_id = $2
ORDER BY position ASC, lower(workflow_file), workflow_file;

-- name: PinWorkflowForUserRepo :one
WITH next_position AS (
    SELECT LEAST(COALESCE(MAX(position), 0) + 1, 1000)::integer AS position
    FROM user_action_workflow_pins
    WHERE user_id = sqlc.arg(user_id)::bigint
      AND repo_id = sqlc.arg(repo_id)::bigint
)
INSERT INTO user_action_workflow_pins (
    user_id, repo_id, workflow_file, position
)
SELECT sqlc.arg(user_id)::bigint,
       sqlc.arg(repo_id)::bigint,
       sqlc.arg(workflow_file)::text,
       next_position.position
FROM next_position
ON CONFLICT (user_id, repo_id, workflow_file) DO UPDATE
SET workflow_file = EXCLUDED.workflow_file
RETURNING user_id, repo_id, workflow_file, position, created_at, updated_at;

-- name: UnpinWorkflowForUserRepo :execrows
DELETE FROM user_action_workflow_pins
WHERE user_id = $1 AND repo_id = $2 AND workflow_file = $3;

-- name: UpdateWorkflowPinPosition :execrows
UPDATE user_action_workflow_pins
SET position = $4
WHERE user_id = $1 AND repo_id = $2 AND workflow_file = $3;
