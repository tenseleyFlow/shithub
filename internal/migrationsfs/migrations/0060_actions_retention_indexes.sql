-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41g retention sweep support. The cleanup worker walks terminal
-- Actions rows by completion time, so give it narrow indexes that do
-- not bloat the hot queued/running paths.

-- +goose Up

CREATE INDEX workflow_steps_retention_idx
    ON workflow_steps (completed_at)
    WHERE completed_at IS NOT NULL
      AND status IN ('completed', 'cancelled', 'skipped');

CREATE INDEX workflow_runs_retention_idx
    ON workflow_runs (completed_at)
    WHERE pinned = false
      AND completed_at IS NOT NULL
      AND status IN ('completed', 'cancelled');

-- +goose Down

DROP INDEX IF EXISTS workflow_runs_retention_idx;
DROP INDEX IF EXISTS workflow_steps_retention_idx;
