-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a-M2 audit follow-up: add step_with column to workflow_steps.
--
-- The parser populates Step.With map[string]Value with the YAML
-- `with:` block contents — forwarded to the runner's alias-specific
-- step (e.g., shithub/upload-artifact@v1's `name:`+`path:` inputs).
-- Migration 0044 had no column to persist this, so the parser model
-- diverged from the schema; S41b/c couldn't INSERT a step with
-- `with:` data.
--
-- Adds the column with a default empty object so existing inserts
-- (the InsertWorkflowStep query updated in this PR) keep working,
-- and so any rows created before this migration runs (none in
-- production yet — S41b hasn't started) backfill harmlessly.
--
-- Defaulted, NOT NULL, jsonb. Same shape as step_env.

-- +goose Up

ALTER TABLE workflow_steps
    ADD COLUMN step_with jsonb NOT NULL DEFAULT '{}'::jsonb;


-- +goose Down

ALTER TABLE workflow_steps
    DROP COLUMN step_with;
