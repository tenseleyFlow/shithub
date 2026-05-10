-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: MarkRunnerJWTUsed :one
INSERT INTO runner_jwt_used (jti, runner_id, job_id, run_id, repo_id, expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (jti) DO NOTHING
RETURNING jti, runner_id, job_id, run_id, repo_id, expires_at, used_at;

-- name: DeleteExpiredRunnerJWTUses :exec
DELETE FROM runner_jwt_used WHERE expires_at < now();
