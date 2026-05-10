-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow runs — top-level row per triggered workflow.
--
-- Each push / pull_request / schedule / workflow_dispatch event matched
-- against a parsed `.shithub/workflows/*.yml` produces one row here.
-- Child rows live in workflow_jobs (0043) and workflow_steps (0044).
-- Status moves queued → running → completed | cancelled. conclusion is
-- set on completion via the same enum values check_runs uses (S24);
-- one workflow_jobs row maps to one check_runs row (S41b creates the
-- mapping).
--
-- Per-repo run_index gives stable URLs (/owner/repo/actions/runs/42)
-- without leaking global IDs across repos. Pattern cribbed from
-- Forgejo's actions_run.index — see .refs/forgejo/models/actions/run.go.
--
-- Optimistic-lock `version` column lets the runner + the cancel path
-- update status concurrently without overwriting each other (Forgejo
-- pattern, same file).
--
-- concurrency_group is parsed in S41a but only honored from S41g; we
-- carry the column from day one so retroactive backfill isn't needed.
-- Fork-PR approval flow (need_approval, approved_by) is parked for
-- v2 but the columns exist so the schema doesn't churn later.

-- +goose Up

CREATE TYPE workflow_run_status AS ENUM (
    'queued', 'running', 'completed', 'cancelled'
);

CREATE TYPE workflow_run_event AS ENUM (
    'push', 'pull_request', 'schedule', 'workflow_dispatch'
);

CREATE TABLE workflow_runs (
    id                  bigserial             PRIMARY KEY,
    repo_id             bigint                NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    run_index           bigint                NOT NULL,
    workflow_file       text                  NOT NULL,
    workflow_name       text                  NOT NULL DEFAULT '',
    head_sha            text                  NOT NULL,
    head_ref            text                  NOT NULL DEFAULT '',
    event               workflow_run_event    NOT NULL,
    event_payload       jsonb                 NOT NULL DEFAULT '{}'::jsonb,
    actor_user_id       bigint                REFERENCES users(id) ON DELETE SET NULL,
    parent_run_id       bigint                REFERENCES workflow_runs(id) ON DELETE SET NULL,
    concurrency_group   text                  NOT NULL DEFAULT '',
    status              workflow_run_status   NOT NULL DEFAULT 'queued',
    conclusion          check_conclusion,
    pinned              boolean               NOT NULL DEFAULT false,
    need_approval       boolean               NOT NULL DEFAULT false,
    approved_by_user_id bigint                REFERENCES users(id) ON DELETE SET NULL,
    started_at          timestamptz,
    completed_at        timestamptz,
    version             integer               NOT NULL DEFAULT 0,
    created_at          timestamptz           NOT NULL DEFAULT now(),
    updated_at          timestamptz           NOT NULL DEFAULT now(),

    UNIQUE (repo_id, run_index),

    CONSTRAINT workflow_runs_workflow_file_length CHECK (char_length(workflow_file) BETWEEN 1 AND 256),
    CONSTRAINT workflow_runs_workflow_name_length CHECK (char_length(workflow_name) <= 256),
    CONSTRAINT workflow_runs_head_sha_format      CHECK (char_length(head_sha) BETWEEN 7 AND 64),
    CONSTRAINT workflow_runs_head_ref_length      CHECK (char_length(head_ref) <= 256),
    CONSTRAINT workflow_runs_concurrency_length   CHECK (char_length(concurrency_group) <= 256),
    CONSTRAINT workflow_runs_completed_has_conclusion CHECK (
        status <> 'completed' OR conclusion IS NOT NULL
    ),
    CONSTRAINT workflow_runs_started_when_running CHECK (
        status NOT IN ('running', 'completed', 'cancelled') OR started_at IS NOT NULL
    ),
    CONSTRAINT workflow_runs_completed_when_done CHECK (
        status NOT IN ('completed', 'cancelled') OR completed_at IS NOT NULL
    )
);

CREATE INDEX workflow_runs_repo_head_idx     ON workflow_runs (repo_id, head_sha);
CREATE INDEX workflow_runs_repo_status_idx   ON workflow_runs (repo_id, status, created_at DESC);
CREATE INDEX workflow_runs_actor_idx         ON workflow_runs (actor_user_id, created_at DESC);
CREATE INDEX workflow_runs_concurrency_idx   ON workflow_runs (repo_id, concurrency_group, status)
    WHERE concurrency_group <> '';
CREATE INDEX workflow_runs_event_idx         ON workflow_runs (repo_id, event, created_at DESC);
CREATE INDEX workflow_runs_parent_idx        ON workflow_runs (parent_run_id)
    WHERE parent_run_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow_runs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


-- +goose Down
DROP TABLE IF EXISTS workflow_runs;
DROP TYPE IF EXISTS workflow_run_event;
DROP TYPE IF EXISTS workflow_run_status;
