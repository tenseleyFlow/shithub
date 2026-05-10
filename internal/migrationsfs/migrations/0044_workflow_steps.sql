-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow steps — one row per `steps[]` entry in a job.
--
-- Steps execute serially within a job. status moves queued → running →
-- completed | cancelled | skipped (skipped fires when an earlier
-- step fails and continue-on-error wasn't set).
--
-- The `run` text is the shell command. The parser (S41a) tags every
-- expression value carried into this string with a Tainted flag; the
-- runner's exec layer (S41d) refuses to interpolate Tainted values
-- into shell strings — they compile to ${SHITHUB_INPUT_*} envvar refs
-- set safely by the runner. We don't need a separate "compiled"
-- column; the render step happens in-runner each execution.
--
-- log_object_key holds the Spaces blob key after the finalize worker
-- (S41d) concatenates and uploads chunks. NULL while running; chunks
-- live in workflow_step_log_chunks (0047) until finalize.
--
-- step_index gives the dispatch order (0-based). step_id (text) is
-- the optional GHA `id:` for cross-step references via
-- ${{ steps.<id>.outputs.X }} (outputs are v2; the column exists now
-- so the parser can carry the id without a schema change).

-- +goose Up

CREATE TYPE workflow_step_status AS ENUM (
    'queued', 'running', 'completed', 'cancelled', 'skipped'
);

CREATE TABLE workflow_steps (
    id                  bigserial             PRIMARY KEY,
    job_id              bigint                NOT NULL REFERENCES workflow_jobs(id) ON DELETE CASCADE,
    step_index          integer               NOT NULL,
    step_id             text                  NOT NULL DEFAULT '',
    step_name           text                  NOT NULL DEFAULT '',
    if_expr             text                  NOT NULL DEFAULT '',
    run_command         text                  NOT NULL DEFAULT '',
    uses_alias          text                  NOT NULL DEFAULT '',
    working_directory   text                  NOT NULL DEFAULT '',
    step_env            jsonb                 NOT NULL DEFAULT '{}'::jsonb,
    continue_on_error   boolean               NOT NULL DEFAULT false,
    status              workflow_step_status  NOT NULL DEFAULT 'queued',
    conclusion          check_conclusion,
    log_object_key      text,
    log_byte_count      bigint                NOT NULL DEFAULT 0,
    started_at          timestamptz,
    completed_at        timestamptz,
    version             integer               NOT NULL DEFAULT 0,
    created_at          timestamptz           NOT NULL DEFAULT now(),
    updated_at          timestamptz           NOT NULL DEFAULT now(),

    UNIQUE (job_id, step_index),

    CONSTRAINT workflow_steps_step_id_format CHECK (
        step_id = '' OR step_id ~ '^[A-Za-z_][A-Za-z0-9_-]*$'
    ),
    CONSTRAINT workflow_steps_name_length CHECK (char_length(step_name) <= 256),
    CONSTRAINT workflow_steps_run_or_uses CHECK (
        (run_command <> '' AND uses_alias = '') OR
        (run_command = '' AND uses_alias <> '')
    ),
    CONSTRAINT workflow_steps_uses_alias_known CHECK (
        uses_alias IN ('', 'actions/checkout@v4',
                       'shithub/upload-artifact@v1',
                       'shithub/download-artifact@v1')
    ),
    CONSTRAINT workflow_steps_working_directory_length CHECK (
        char_length(working_directory) <= 1024
    ),
    CONSTRAINT workflow_steps_completed_has_conclusion CHECK (
        status NOT IN ('completed', 'skipped') OR conclusion IS NOT NULL
    ),
    CONSTRAINT workflow_steps_log_byte_nonnegative CHECK (log_byte_count >= 0)
);

CREATE INDEX workflow_steps_job_idx ON workflow_steps (job_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow_steps
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


-- +goose Down
DROP TABLE IF EXISTS workflow_steps;
DROP TYPE IF EXISTS workflow_step_status;
