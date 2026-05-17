-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Viewer-scoped Actions workflow sidebar pins. Workflows are still
-- discovered from git/workflow_runs; this table stores only a user's
-- display preference for a repo-relative workflow file path.

-- +goose Up
CREATE TABLE user_action_workflow_pins (
    user_id       bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id       bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    workflow_file text        NOT NULL,
    position      integer     NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, repo_id, workflow_file),
    CONSTRAINT user_action_workflow_pins_file_format
        CHECK (
            workflow_file LIKE '.shithub/workflows/%'
            AND char_length(workflow_file) BETWEEN 1 AND 256
        ),
    CONSTRAINT user_action_workflow_pins_position_range
        CHECK (position BETWEEN 1 AND 1000)
);

CREATE INDEX user_action_workflow_pins_user_repo_position_idx
    ON user_action_workflow_pins (user_id, repo_id, position, workflow_file);

CREATE INDEX user_action_workflow_pins_repo_idx
    ON user_action_workflow_pins (repo_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON user_action_workflow_pins
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TRIGGER IF EXISTS set_updated_at ON user_action_workflow_pins;
DROP TABLE IF EXISTS user_action_workflow_pins;
