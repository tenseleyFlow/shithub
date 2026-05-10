-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41c runner JWT replay protection.
--
-- The runner heartbeat endpoint mints short-lived, per-job JWTs after a
-- registration-token-authenticated runner claims a workflow_jobs row. Job
-- endpoints require that JWT and consume its jti exactly once. INSERT ...
-- ON CONFLICT DO NOTHING against this table is the replay gate: one affected
-- row means "first use"; zero rows means "replay" and the API returns 401.
--
-- We keep the workflow references here for auditability and cleanup. jti is
-- the hot lookup path and is enforced by the PRIMARY KEY.

-- +goose Up

CREATE TABLE runner_jwt_used (
    jti         text         PRIMARY KEY,
    runner_id   bigint       NOT NULL REFERENCES workflow_runners(id) ON DELETE CASCADE,
    job_id      bigint       NOT NULL REFERENCES workflow_jobs(id) ON DELETE CASCADE,
    run_id      bigint       NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    repo_id     bigint       NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    expires_at  timestamptz  NOT NULL,
    used_at     timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT runner_jwt_used_jti_length CHECK (char_length(jti) BETWEEN 16 AND 128)
);

CREATE INDEX runner_jwt_used_expires_idx
    ON runner_jwt_used (expires_at);
CREATE INDEX runner_jwt_used_runner_used_idx
    ON runner_jwt_used (runner_id, used_at DESC);


-- +goose Down
DROP TABLE IF EXISTS runner_jwt_used;
