-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP23 repository environments.
--
-- GitHub's Actions environments are a repo-owned surface used by jobs via
-- `jobs.<job>.environment`. The environment row owns protection settings,
-- deployment branch policy, and environment-scoped secrets. This migration
-- installs the durable substrate first; later SP23 slices wire UI controls
-- and protection approval enforcement on top.
--
-- workflow_jobs deliberately stores the parsed environment name/url as text
-- rather than a FK because GitHub permits workflow-authored environment
-- names, including expression-shaped values. A repo_environment row exists
-- only when the owner has configured protection/secrets for that name.
--
-- Down-migration note: environment-scoped workflow_secrets are deleted before
-- the owner XOR is restored to the repo/org/user scopes.

-- +goose Up

CREATE TYPE repo_environment_deployment_branch_policy AS ENUM (
    'all',
    'protected',
    'selected'
);

CREATE TABLE repo_environments (
    id                         bigserial PRIMARY KEY,
    repo_id                    bigint NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    name                       citext NOT NULL,
    required_reviewers_enabled boolean NOT NULL DEFAULT false,
    prevent_self_review        boolean NOT NULL DEFAULT false,
    wait_timer_minutes         integer NOT NULL DEFAULT 0,
    deployment_branch_policy   repo_environment_deployment_branch_policy NOT NULL DEFAULT 'all',
    created_at                 timestamptz NOT NULL DEFAULT now(),
    updated_at                 timestamptz NOT NULL DEFAULT now(),

    UNIQUE (repo_id, name),

    CONSTRAINT repo_environments_name_length CHECK (char_length(name::text) BETWEEN 1 AND 255),
    CONSTRAINT repo_environments_wait_timer_range CHECK (wait_timer_minutes BETWEEN 0 AND 43200)
);

CREATE INDEX repo_environments_repo_idx ON repo_environments (repo_id, name);

CREATE TABLE repo_environment_deployment_branches (
    id             bigserial PRIMARY KEY,
    environment_id bigint NOT NULL REFERENCES repo_environments(id) ON DELETE CASCADE,
    pattern        text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),

    UNIQUE (environment_id, pattern),

    CONSTRAINT repo_environment_deployment_branches_pattern_length CHECK (
        char_length(pattern) BETWEEN 1 AND 255
    )
);

ALTER TABLE workflow_jobs
    ADD COLUMN environment_name text NOT NULL DEFAULT '',
    ADD COLUMN environment_url text NOT NULL DEFAULT '';

ALTER TABLE workflow_jobs
    ADD CONSTRAINT workflow_jobs_environment_name_length CHECK (char_length(environment_name) <= 255),
    ADD CONSTRAINT workflow_jobs_environment_url_length CHECK (char_length(environment_url) <= 2048);

ALTER TABLE workflow_secrets
    ADD COLUMN environment_id bigint REFERENCES repo_environments(id) ON DELETE CASCADE;

ALTER TABLE workflow_secrets DROP CONSTRAINT workflow_secrets_owner_xor;
ALTER TABLE workflow_secrets ADD CONSTRAINT workflow_secrets_owner_xor CHECK (
    (repo_id IS NOT NULL AND org_id IS NULL     AND user_id IS NULL     AND environment_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NOT NULL AND user_id IS NULL     AND environment_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NULL     AND user_id IS NOT NULL AND environment_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NULL     AND user_id IS NULL     AND environment_id IS NOT NULL)
);

CREATE UNIQUE INDEX workflow_secrets_environment_name_idx
    ON workflow_secrets (environment_id, name) WHERE environment_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON repo_environments
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down

DROP TRIGGER IF EXISTS set_updated_at ON repo_environments;

DROP INDEX IF EXISTS workflow_secrets_environment_name_idx;
DELETE FROM workflow_secrets WHERE environment_id IS NOT NULL;
ALTER TABLE workflow_secrets DROP CONSTRAINT workflow_secrets_owner_xor;
ALTER TABLE workflow_secrets ADD CONSTRAINT workflow_secrets_owner_xor CHECK (
    (repo_id IS NOT NULL AND org_id IS NULL     AND user_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NOT NULL AND user_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NULL     AND user_id IS NOT NULL)
);
ALTER TABLE workflow_secrets DROP COLUMN environment_id;

ALTER TABLE workflow_jobs DROP CONSTRAINT IF EXISTS workflow_jobs_environment_url_length;
ALTER TABLE workflow_jobs DROP CONSTRAINT IF EXISTS workflow_jobs_environment_name_length;
ALTER TABLE workflow_jobs DROP COLUMN environment_url;
ALTER TABLE workflow_jobs DROP COLUMN environment_name;

DROP TABLE IF EXISTS repo_environment_deployment_branches;
DROP TABLE IF EXISTS repo_environments;
DROP TYPE IF EXISTS repo_environment_deployment_branch_policy;
