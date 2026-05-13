-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41j runner pool operations: runner metadata plus explicit drain/revoke
-- state. Draining prevents new claims while allowing in-flight jobs to finish.
-- Revocation is a hard operator control: registration tokens no longer
-- authenticate and job API JWTs minted for that runner are rejected.

-- +goose Up

ALTER TABLE workflow_runners
    ADD COLUMN host_name text NOT NULL DEFAULT '',
    ADD COLUMN version text NOT NULL DEFAULT '',
    ADD COLUMN draining_at timestamptz,
    ADD COLUMN drain_reason text NOT NULL DEFAULT '',
    ADD COLUMN revoked_at timestamptz,
    ADD COLUMN revoked_reason text NOT NULL DEFAULT '',
    ADD CONSTRAINT workflow_runners_host_name_length CHECK (char_length(host_name) <= 255),
    ADD CONSTRAINT workflow_runners_version_length CHECK (char_length(version) <= 255),
    ADD CONSTRAINT workflow_runners_drain_reason_length CHECK (char_length(drain_reason) <= 1000),
    ADD CONSTRAINT workflow_runners_revoked_reason_length CHECK (char_length(revoked_reason) <= 1000);

CREATE INDEX workflow_runners_draining_idx
    ON workflow_runners (draining_at)
    WHERE draining_at IS NOT NULL AND revoked_at IS NULL;

CREATE INDEX workflow_runners_revoked_idx
    ON workflow_runners (revoked_at)
    WHERE revoked_at IS NOT NULL;

CREATE INDEX workflow_runners_claimable_idx
    ON workflow_runners (status)
    WHERE revoked_at IS NULL AND draining_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS workflow_runners_claimable_idx;
DROP INDEX IF EXISTS workflow_runners_revoked_idx;
DROP INDEX IF EXISTS workflow_runners_draining_idx;

ALTER TABLE workflow_runners
    DROP CONSTRAINT IF EXISTS workflow_runners_revoked_reason_length,
    DROP CONSTRAINT IF EXISTS workflow_runners_drain_reason_length,
    DROP CONSTRAINT IF EXISTS workflow_runners_version_length,
    DROP CONSTRAINT IF EXISTS workflow_runners_host_name_length,
    DROP COLUMN IF EXISTS revoked_reason,
    DROP COLUMN IF EXISTS revoked_at,
    DROP COLUMN IF EXISTS drain_reason,
    DROP COLUMN IF EXISTS draining_at,
    DROP COLUMN IF EXISTS version,
    DROP COLUMN IF EXISTS host_name;
