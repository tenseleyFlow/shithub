-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow jobs — one row per `jobs.<id>` block in the workflow.
--
-- Each row pairs 1:1 with a check_runs row created in S41b
-- (app_slug='shithub-actions'); status transitions on this table cascade
-- to that row via the suite-rollup logic in internal/checks/. Status
-- moves queued → running → completed | cancelled. conclusion uses the
-- existing check_conclusion enum from 0025.
--
-- runner_id binds a claimed job to the runner that's executing it
-- (workflow_runners table, 0046). NULL = not yet claimed. cancel_requested
-- is the boolean the runner heartbeat checks (S41g); we add it from day
-- one so the runner protocol doesn't need a column-add later.
--
-- needs_jobs[] mirrors GHA's `needs:` — the names (not IDs) of
-- prerequisite jobs in the same run. Resolved at trigger time (S41b)
-- against same-run job names; cycles + dangling refs caught by the
-- parser (S41a expr/eval).
--
-- if_expr is the parsed-but-unevaluated `if:` expression carried as
-- text; the evaluator (S41a) consumes it at dispatch time.
--
-- timeout_minutes mirrors GHA semantics; runner enforcement lands in
-- S41g but we carry the column now.

-- +goose Up

CREATE TYPE workflow_job_status AS ENUM (
    'queued', 'running', 'completed', 'cancelled', 'skipped'
);

CREATE TABLE workflow_jobs (
    id                  bigserial             PRIMARY KEY,
    run_id              bigint                NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    job_index           integer               NOT NULL,
    job_key             text                  NOT NULL,
    job_name            text                  NOT NULL DEFAULT '',
    runs_on             text                  NOT NULL DEFAULT '',
    runner_id           bigint,
    needs_jobs          text[]                NOT NULL DEFAULT ARRAY[]::text[],
    if_expr             text                  NOT NULL DEFAULT '',
    timeout_minutes     integer               NOT NULL DEFAULT 360,
    permissions         jsonb                 NOT NULL DEFAULT '{}'::jsonb,
    job_env             jsonb                 NOT NULL DEFAULT '{}'::jsonb,
    status              workflow_job_status   NOT NULL DEFAULT 'queued',
    conclusion          check_conclusion,
    cancel_requested    boolean               NOT NULL DEFAULT false,
    started_at          timestamptz,
    completed_at        timestamptz,
    version             integer               NOT NULL DEFAULT 0,
    created_at          timestamptz           NOT NULL DEFAULT now(),
    updated_at          timestamptz           NOT NULL DEFAULT now(),

    UNIQUE (run_id, job_key),

    CONSTRAINT workflow_jobs_job_key_format CHECK (
        char_length(job_key) BETWEEN 1 AND 100
        AND job_key ~ '^[A-Za-z_][A-Za-z0-9_-]*$'
    ),
    CONSTRAINT workflow_jobs_job_name_length CHECK (char_length(job_name) <= 256),
    CONSTRAINT workflow_jobs_runs_on_length  CHECK (char_length(runs_on) <= 256),
    CONSTRAINT workflow_jobs_timeout_range   CHECK (timeout_minutes BETWEEN 1 AND 4320),
    CONSTRAINT workflow_jobs_completed_has_conclusion CHECK (
        status NOT IN ('completed', 'skipped') OR conclusion IS NOT NULL
    ),
    CONSTRAINT workflow_jobs_runner_when_running CHECK (
        status NOT IN ('running', 'completed') OR runner_id IS NOT NULL
    )
);

CREATE INDEX workflow_jobs_run_idx          ON workflow_jobs (run_id);
CREATE INDEX workflow_jobs_runner_idx       ON workflow_jobs (runner_id, status)
    WHERE runner_id IS NOT NULL;
CREATE INDEX workflow_jobs_status_idx       ON workflow_jobs (status, created_at)
    WHERE status IN ('queued', 'running');

CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow_jobs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


-- +goose Down
DROP TABLE IF EXISTS workflow_jobs;
DROP TYPE IF EXISTS workflow_job_status;
