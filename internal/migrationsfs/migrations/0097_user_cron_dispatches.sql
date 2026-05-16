-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-13b: per-user cron-scheduled workflow_dispatch.
--
-- A Pro user can schedule a workflow to fire on a cron cadence
-- without committing a `schedule:` block to the workflow file. Each
-- row is one (workflow_file, ref, cron_expr) bound to a user-owned
-- repo. The sweep job (cron_workflow_dispatch:sweep) claims due rows
-- via FOR UPDATE SKIP LOCKED, calls trigger.Enqueue with EventKind=
-- schedule, then advances next_fire_at = nextTick(cron_expr).
--
-- Why the ON DELETE CASCADE chain:
--   * user_id: if the owner is deleted, all their schedules go too.
--   * repo_id: a deleted repo can never fire; cascading prevents
--     orphan rows that the sweep would have to repeatedly skip.
--
-- Cron expression syntax is standard 5-field crontab in UTC:
--     minute hour day-of-month month day-of-week
-- Parsed at create time via robfig/cron/v3; persisted as the raw
-- text the user supplied so the next sweep can re-derive next_fire_at
-- without round-tripping through a normalized form.
--
-- Last-fire metadata is two columns rather than a foreign key into
-- workflow_runs because a cron tick may be skipped (entitlement deny,
-- branch missing, workflow file deleted) — we still want to record the
-- attempt without enqueueing a run.

-- +goose Up
CREATE TYPE user_cron_dispatch_last_status AS ENUM (
    'pending', 'fired', 'skipped_entitlement', 'skipped_missing_ref',
    'skipped_missing_workflow', 'skipped_parse_error', 'error'
);

CREATE TABLE user_cron_dispatches (
    id                 BIGSERIAL PRIMARY KEY,
    user_id            BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id            BIGINT NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    workflow_file      TEXT NOT NULL,
    ref                TEXT NOT NULL,
    cron_expr          TEXT NOT NULL,
    next_fire_at       TIMESTAMPTZ NOT NULL,
    last_fire_at       TIMESTAMPTZ,
    last_fire_status   user_cron_dispatch_last_status NOT NULL DEFAULT 'pending',
    last_fire_error    TEXT,
    disabled_at        TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX user_cron_dispatches_user_id_idx
    ON user_cron_dispatches (user_id);

-- Partial index drives the sweep: only un-disabled rows past due.
CREATE INDEX user_cron_dispatches_due_idx
    ON user_cron_dispatches (next_fire_at)
    WHERE disabled_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS user_cron_dispatches;
DROP TYPE IF EXISTS user_cron_dispatch_last_status;
