-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow runners + per-runner registration tokens.
--
-- workflow_runners — one row per registered runner (operator runs
-- `shithubd admin runner register --name foo --labels self-hosted,linux`).
-- The labels[] array is what `runs-on:` matches against. last_heartbeat_at
-- is updated by the runner's heartbeat endpoint (S41c); rows where
-- last_heartbeat_at is older than `runner.heartbeat_timeout` (config,
-- default 60s) are considered offline.
--
-- runner_tokens — registration tokens minted at admin-register time.
-- The plaintext token is shown to the operator once and never persisted;
-- token_hash is what we store + compare on heartbeat. expires_at NULL
-- means the registration token doesn't expire (long-lived); when set,
-- the token is rejected after that point. revoked_at is the operator's
-- revoke knob (`shithubd admin runner revoke --id N`).
--
-- The per-job JWT (15-min, single-use, scoped to one workflow_jobs.id)
-- is a separate construct minted in-memory and validated against
-- runner_jwt_used (S41c migration); it does NOT live in this table.

-- +goose Up

CREATE TYPE workflow_runner_status AS ENUM ('idle', 'busy', 'offline');

CREATE TABLE workflow_runners (
    id                       bigserial               PRIMARY KEY,
    name                     text                    NOT NULL,
    labels                   text[]                  NOT NULL DEFAULT ARRAY[]::text[],
    capacity                 integer                 NOT NULL DEFAULT 1,
    status                   workflow_runner_status  NOT NULL DEFAULT 'offline',
    last_heartbeat_at        timestamptz,
    registered_by_user_id    bigint                  REFERENCES users(id) ON DELETE SET NULL,
    created_at               timestamptz             NOT NULL DEFAULT now(),
    updated_at               timestamptz             NOT NULL DEFAULT now(),

    CONSTRAINT workflow_runners_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT workflow_runners_name_format CHECK (name ~ '^[A-Za-z0-9_-]+$'),
    CONSTRAINT workflow_runners_capacity_range CHECK (capacity BETWEEN 1 AND 64)
);

CREATE UNIQUE INDEX workflow_runners_name_idx ON workflow_runners (name);
CREATE INDEX workflow_runners_status_idx      ON workflow_runners (status);
CREATE INDEX workflow_runners_labels_idx      ON workflow_runners USING GIN (labels);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow_runners
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


CREATE TABLE runner_tokens (
    id          bigserial   PRIMARY KEY,
    runner_id   bigint      NOT NULL REFERENCES workflow_runners(id) ON DELETE CASCADE,
    token_hash  bytea       NOT NULL,
    expires_at  timestamptz,
    revoked_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT runner_tokens_token_hash_length CHECK (octet_length(token_hash) = 32)
);

CREATE UNIQUE INDEX runner_tokens_hash_idx ON runner_tokens (token_hash);
CREATE INDEX runner_tokens_runner_idx     ON runner_tokens (runner_id);


-- The forward reference from workflow_jobs.runner_id to workflow_runners.id
-- couldn't be expressed in 0043 because that table didn't exist yet.
-- Add the FK now that both tables are in place.
ALTER TABLE workflow_jobs
    ADD CONSTRAINT workflow_jobs_runner_id_fkey
        FOREIGN KEY (runner_id) REFERENCES workflow_runners(id) ON DELETE SET NULL;


-- +goose Down
ALTER TABLE workflow_jobs
    DROP CONSTRAINT IF EXISTS workflow_jobs_runner_id_fkey;
DROP TABLE IF EXISTS runner_tokens;
DROP TABLE IF EXISTS workflow_runners;
DROP TYPE IF EXISTS workflow_runner_status;
