-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Per-workflow enable/disable knob for the §13 REST surface.
--
-- We don't have a `workflows` table (workflows are discovered from
-- the git tree on demand), so the discriminator here is the
-- repo-relative path (e.g. ".shithub/workflows/ci.yml"). One row per
-- disabled workflow file; absence of a row means the workflow runs
-- normally.
--
-- The trigger pipeline consults this table on every event: a disabled
-- workflow's runs are not enqueued. Re-enabling (DELETE the row) is
-- the inverse — the next matching event resumes triggering as
-- normal. This is intentionally a flag, not an audit log; the
-- `audit_log` table records the actor + reason for the operator
-- forensics path.

-- +goose Up
CREATE TABLE workflow_disabled (
    repo_id            bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    workflow_file      text        NOT NULL,
    disabled_by_user_id bigint     REFERENCES users(id) ON DELETE SET NULL,
    disabled_at        timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (repo_id, workflow_file),
    CONSTRAINT workflow_disabled_file_format
        CHECK (workflow_file LIKE '.shithub/workflows/%')
);

CREATE INDEX workflow_disabled_repo_id_idx
    ON workflow_disabled (repo_id);

-- +goose Down
DROP TABLE IF EXISTS workflow_disabled;
