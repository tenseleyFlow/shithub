-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41b trigger pipeline: idempotency on the *triggering event*.
--
-- Each push, PR transition, dispatch click, or cron tick is a unique
-- triggering event with a stable identifier constructed by the
-- enqueueing site:
--
--   - push_process       → "push:<push_event_id>"
--   - pr open/synchronize → "pr_<action>:<pr_id>:<head_sha>"
--   - workflow_dispatch  → "dispatch:<workflow_id>:<request_uuid>"
--   - schedule sweep     → "schedule:<workflow_id>:<window_start_unix>"
--
-- The trigger handler's INSERT … ON CONFLICT DO NOTHING uses the
-- partial UNIQUE index below to dedupe across worker retries (e.g.,
-- push_process retried after a transient error) and admin replays
-- (operators using `admin run-job workflow:trigger ...`). One
-- triggering event → one workflow_runs row per matched workflow file.
--
-- Re-runs (the future "Re-run" button) explicitly produce a NEW
-- trigger_event_id (e.g. "rerun:<original_run_id>:<request_uuid>"), so
-- they don't collide and `parent_run_id` chains them back. History is
-- preserved.
--
-- DEFAULT '' on the column lets the migration apply against any
-- pre-existing rows (none yet — S41a shipped the schema but nothing
-- populates it). The partial UNIQUE excludes the empty string so
-- those backfilled rows don't constrain each other; new code is
-- responsible for always setting a non-empty value.

-- +goose Up

ALTER TABLE workflow_runs
    ADD COLUMN trigger_event_id text NOT NULL DEFAULT '';

CREATE UNIQUE INDEX workflow_runs_trigger_event_idx
    ON workflow_runs (repo_id, workflow_file, trigger_event_id)
    WHERE trigger_event_id <> '';


-- +goose Down

DROP INDEX IF EXISTS workflow_runs_trigger_event_idx;
ALTER TABLE workflow_runs DROP COLUMN trigger_event_id;
