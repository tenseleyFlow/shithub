-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateCronDispatch :one
INSERT INTO user_cron_dispatches (
    user_id, repo_id, workflow_file, ref, cron_expr, next_fire_at
)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, user_id, repo_id, workflow_file, ref, cron_expr,
          next_fire_at, last_fire_at, last_fire_status, last_fire_error,
          disabled_at, created_at, updated_at;

-- name: GetCronDispatchByID :one
SELECT id, user_id, repo_id, workflow_file, ref, cron_expr,
       next_fire_at, last_fire_at, last_fire_status, last_fire_error,
       disabled_at, created_at, updated_at
FROM user_cron_dispatches
WHERE id = $1;

-- name: ListCronDispatchesForUser :many
SELECT id, user_id, repo_id, workflow_file, ref, cron_expr,
       next_fire_at, last_fire_at, last_fire_status, last_fire_error,
       disabled_at, created_at, updated_at
FROM user_cron_dispatches
WHERE user_id = $1
ORDER BY created_at DESC;

-- name: DisableCronDispatch :exec
UPDATE user_cron_dispatches
SET disabled_at = now(), updated_at = now()
WHERE id = $1;

-- name: DeleteCronDispatch :exec
DELETE FROM user_cron_dispatches WHERE id = $1;

-- name: ClaimDueCronDispatches :many
-- Claims up to $1 due, un-disabled rows in oldest-first order. SKIP
-- LOCKED + a UPDATE-style claim would require a status column to flip;
-- since the sweep advances next_fire_at after dispatch, we just SELECT
-- FOR UPDATE inside the worker tx — the caller wraps each row's
-- dispatch + advance in a separate sub-tx so a slow fire doesn't hold
-- the lock on the whole batch.
SELECT id, user_id, repo_id, workflow_file, ref, cron_expr,
       next_fire_at, last_fire_at, last_fire_status, last_fire_error,
       disabled_at, created_at, updated_at
FROM user_cron_dispatches
WHERE disabled_at IS NULL
  AND next_fire_at <= now()
ORDER BY next_fire_at ASC
LIMIT $1
FOR UPDATE SKIP LOCKED;

-- name: AdvanceCronDispatch :exec
-- Records a fire (advances next, sets last). Called regardless of
-- whether the fire actually enqueued a run — sweeps that bail (missing
-- workflow file, entitlement denied) still advance to avoid spinning
-- on the same due row every tick.
UPDATE user_cron_dispatches
SET next_fire_at = $2,
    last_fire_at = now(),
    last_fire_status = $3,
    last_fire_error = $4,
    updated_at = now()
WHERE id = $1;
