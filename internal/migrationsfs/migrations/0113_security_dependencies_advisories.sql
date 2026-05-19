-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25: repository dependency inventory, dependency advisories, and
-- repository security advisories. The inventory is repo-owned so org
-- security overview pages can aggregate without walking git history on
-- request paths.
--
-- Ecosystem values are deliberately text, not enums. Parser support will
-- expand over time and should not require an enum migration for every new
-- package manager.

-- +goose Up
CREATE TABLE repo_dependency_snapshots (
    repo_id          bigint      PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    default_branch   text        NOT NULL,
    head_sha         text        NOT NULL,
    manifest_count   int         NOT NULL DEFAULT 0,
    dependency_count int         NOT NULL DEFAULT 0,
    generated_at     timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_dependency_snapshots_default_branch_shape CHECK (char_length(default_branch) BETWEEN 1 AND 255),
    CONSTRAINT repo_dependency_snapshots_head_sha_shape CHECK (char_length(head_sha) BETWEEN 1 AND 128),
    CONSTRAINT repo_dependency_snapshots_manifest_count_nonnegative CHECK (manifest_count >= 0),
    CONSTRAINT repo_dependency_snapshots_dependency_count_nonnegative CHECK (dependency_count >= 0)
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON repo_dependency_snapshots
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE repo_dependencies (
    id              bigserial   PRIMARY KEY,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    ecosystem       text        NOT NULL,
    package_name    text        NOT NULL,
    package_version text        NOT NULL DEFAULT '',
    manifest_path   text        NOT NULL,
    lockfile_path   text        NOT NULL DEFAULT '',
    scope           text        NOT NULL DEFAULT '',
    direct          boolean     NOT NULL DEFAULT true,
    package_manager text        NOT NULL DEFAULT '',
    source          text        NOT NULL DEFAULT '',
    last_seen_sha   text        NOT NULL,
    first_seen_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at    timestamptz NOT NULL DEFAULT now(),
    stale_at        timestamptz NULL,

    CONSTRAINT repo_dependencies_ecosystem_shape CHECK (char_length(ecosystem) BETWEEN 1 AND 40),
    CONSTRAINT repo_dependencies_package_name_shape CHECK (char_length(package_name) BETWEEN 1 AND 512),
    CONSTRAINT repo_dependencies_package_version_shape CHECK (char_length(package_version) <= 255),
    CONSTRAINT repo_dependencies_manifest_path_shape CHECK (char_length(manifest_path) BETWEEN 1 AND 1024),
    CONSTRAINT repo_dependencies_lockfile_path_shape CHECK (char_length(lockfile_path) <= 1024),
    CONSTRAINT repo_dependencies_scope_shape CHECK (char_length(scope) <= 80),
    CONSTRAINT repo_dependencies_package_manager_shape CHECK (char_length(package_manager) <= 80),
    CONSTRAINT repo_dependencies_source_shape CHECK (char_length(source) <= 80),
    CONSTRAINT repo_dependencies_last_seen_sha_shape CHECK (char_length(last_seen_sha) BETWEEN 1 AND 128),
    CONSTRAINT repo_dependencies_unique UNIQUE (repo_id, ecosystem, package_name, manifest_path)
);

CREATE INDEX repo_dependencies_repo_current_idx
    ON repo_dependencies (repo_id, ecosystem, package_name)
    WHERE stale_at IS NULL;

CREATE INDEX repo_dependencies_repo_stale_idx
    ON repo_dependencies (repo_id, stale_at);

CREATE TABLE dependency_advisories (
    id               bigserial   PRIMARY KEY,
    source           text        NOT NULL,
    external_id      text        NOT NULL,
    ecosystem        text        NOT NULL,
    package_name     text        NOT NULL,
    affected_range   text        NOT NULL DEFAULT '',
    patched_versions text        NOT NULL DEFAULT '',
    severity         text        NOT NULL,
    summary          text        NOT NULL,
    description      text        NOT NULL DEFAULT '',
    reference_urls   jsonb       NOT NULL DEFAULT '[]'::jsonb,
    published_at     timestamptz NULL,
    withdrawn_at     timestamptz NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_advisories_source_shape CHECK (char_length(source) BETWEEN 1 AND 80),
    CONSTRAINT dependency_advisories_external_id_shape CHECK (char_length(external_id) BETWEEN 1 AND 120),
    CONSTRAINT dependency_advisories_ecosystem_shape CHECK (char_length(ecosystem) BETWEEN 1 AND 40),
    CONSTRAINT dependency_advisories_package_name_shape CHECK (char_length(package_name) BETWEEN 1 AND 512),
    CONSTRAINT dependency_advisories_severity_known CHECK (severity IN ('low', 'moderate', 'high', 'critical')),
    CONSTRAINT dependency_advisories_summary_shape CHECK (char_length(summary) BETWEEN 1 AND 500),
    CONSTRAINT dependency_advisories_reference_urls_array CHECK (jsonb_typeof(reference_urls) = 'array'),
    CONSTRAINT dependency_advisories_unique UNIQUE (source, external_id)
);

CREATE INDEX dependency_advisories_package_idx
    ON dependency_advisories (ecosystem, lower(package_name))
    WHERE withdrawn_at IS NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_advisories
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE repo_dependency_alerts (
    id             bigserial   PRIMARY KEY,
    repo_id        bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    dependency_id  bigint      NOT NULL REFERENCES repo_dependencies(id) ON DELETE CASCADE,
    advisory_id    bigint      NOT NULL REFERENCES dependency_advisories(id) ON DELETE CASCADE,
    status         text        NOT NULL DEFAULT 'open',
    dismissal_note text        NOT NULL DEFAULT '',
    dismissed_by   bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    dismissed_at   timestamptz NULL,
    resolved_at    timestamptz NULL,
    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_dependency_alerts_status_known CHECK (status IN ('open', 'dismissed', 'resolved')),
    CONSTRAINT repo_dependency_alerts_dismissal_note_shape CHECK (char_length(dismissal_note) <= 500),
    CONSTRAINT repo_dependency_alerts_unique UNIQUE (repo_id, dependency_id, advisory_id)
);

CREATE INDEX repo_dependency_alerts_repo_status_idx
    ON repo_dependency_alerts (repo_id, status, last_seen_at DESC);

CREATE INDEX repo_dependency_alerts_advisory_idx
    ON repo_dependency_alerts (advisory_id);

CREATE TABLE repo_security_advisories (
    id                  bigserial   PRIMARY KEY,
    repo_id             bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    identifier          text        NOT NULL,
    state               text        NOT NULL DEFAULT 'draft',
    severity            text        NOT NULL,
    summary             text        NOT NULL,
    description         text        NOT NULL DEFAULT '',
    affected_ecosystem  text        NOT NULL DEFAULT '',
    affected_package    text        NOT NULL DEFAULT '',
    vulnerable_versions text        NOT NULL DEFAULT '',
    patched_versions    text        NOT NULL DEFAULT '',
    created_by          bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    published_at        timestamptz NULL,
    closed_at           timestamptz NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT repo_security_advisories_identifier_shape CHECK (char_length(identifier) BETWEEN 1 AND 120),
    CONSTRAINT repo_security_advisories_state_known CHECK (state IN ('draft', 'published', 'closed')),
    CONSTRAINT repo_security_advisories_severity_known CHECK (severity IN ('low', 'moderate', 'high', 'critical')),
    CONSTRAINT repo_security_advisories_summary_shape CHECK (char_length(summary) BETWEEN 1 AND 500),
    CONSTRAINT repo_security_advisories_unique UNIQUE (repo_id, identifier)
);

CREATE INDEX repo_security_advisories_repo_state_idx
    ON repo_security_advisories (repo_id, state, updated_at DESC);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON repo_security_advisories
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS repo_security_advisories;
DROP TABLE IF EXISTS repo_dependency_alerts;
DROP TABLE IF EXISTS dependency_advisories;
DROP TABLE IF EXISTS repo_dependencies;
DROP TABLE IF EXISTS repo_dependency_snapshots;
