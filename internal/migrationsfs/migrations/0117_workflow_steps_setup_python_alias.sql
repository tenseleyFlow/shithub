-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41n-2: reserve actions/setup-python@v5 as a first-party executable
-- compatibility shim. This is not general marketplace action support.

-- +goose Up

ALTER TABLE workflow_steps
    DROP CONSTRAINT workflow_steps_uses_alias_known;

ALTER TABLE workflow_steps
    ADD CONSTRAINT workflow_steps_uses_alias_known CHECK (
        uses_alias IN ('', 'actions/checkout@v4',
                       'actions/setup-python@v5',
                       'shithub/upload-artifact@v1',
                       'shithub/download-artifact@v1')
    );

-- +goose Down

ALTER TABLE workflow_steps
    DROP CONSTRAINT workflow_steps_uses_alias_known;

ALTER TABLE workflow_steps
    ADD CONSTRAINT workflow_steps_uses_alias_known CHECK (
        uses_alias IN ('', 'actions/checkout@v4',
                       'shithub/upload-artifact@v1',
                       'shithub/download-artifact@v1')
    );
