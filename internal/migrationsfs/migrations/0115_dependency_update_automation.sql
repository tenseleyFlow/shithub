-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP25b: dependency update automation foundation. This stores the
-- Dependabot-compatible repository configuration, bounded scheduler jobs,
-- update PR bookkeeping, and custom auto-triage rules/audit events.

-- +goose Up
CREATE TABLE dependency_update_configs (
    id                      bigserial   PRIMARY KEY,
    repo_id                 bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    ecosystem               text        NOT NULL,
    package_manager         text        NOT NULL,
    directory               text        NOT NULL DEFAULT '/',
    schedule_interval       text        NOT NULL,
    schedule_day            text        NOT NULL DEFAULT '',
    schedule_time           text        NOT NULL DEFAULT '',
    schedule_timezone       text        NOT NULL DEFAULT '',
    schedule_cron           text        NOT NULL DEFAULT '',
    open_pull_request_limit int         NOT NULL DEFAULT 5,
    target_branch           text        NOT NULL DEFAULT '',
    allow_rules             jsonb       NOT NULL DEFAULT '[]'::jsonb,
    ignore_rules            jsonb       NOT NULL DEFAULT '[]'::jsonb,
    groups                  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    registries              jsonb       NOT NULL DEFAULT '[]'::jsonb,
    unsupported_keys        text[]      NOT NULL DEFAULT '{}',
    enabled                 boolean     NOT NULL DEFAULT true,
    raw_config_hash         text        NOT NULL,
    raw_config_path         text        NOT NULL DEFAULT '.github/dependabot.yml',
    last_synced_sha         text        NOT NULL DEFAULT '',
    last_checked_at         timestamptz NULL,
    next_run_at             timestamptz NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_update_configs_ecosystem_shape CHECK (char_length(ecosystem) BETWEEN 1 AND 40),
    CONSTRAINT dependency_update_configs_package_manager_shape CHECK (char_length(package_manager) BETWEEN 1 AND 80),
    CONSTRAINT dependency_update_configs_directory_shape CHECK (char_length(directory) BETWEEN 1 AND 1024 AND left(directory, 1) = '/'),
    CONSTRAINT dependency_update_configs_schedule_interval_known CHECK (schedule_interval IN ('daily', 'weekly', 'monthly', 'quarterly', 'semiannually', 'yearly', 'cron')),
    CONSTRAINT dependency_update_configs_schedule_day_shape CHECK (char_length(schedule_day) <= 20),
    CONSTRAINT dependency_update_configs_schedule_time_shape CHECK (char_length(schedule_time) <= 20),
    CONSTRAINT dependency_update_configs_schedule_timezone_shape CHECK (char_length(schedule_timezone) <= 80),
    CONSTRAINT dependency_update_configs_schedule_cron_shape CHECK (char_length(schedule_cron) <= 120),
    CONSTRAINT dependency_update_configs_cron_requires_cronjob CHECK (schedule_interval <> 'cron' OR schedule_cron <> ''),
    CONSTRAINT dependency_update_configs_open_pr_limit_bounds CHECK (open_pull_request_limit BETWEEN 0 AND 100),
    CONSTRAINT dependency_update_configs_target_branch_shape CHECK (char_length(target_branch) <= 255),
    CONSTRAINT dependency_update_configs_allow_rules_array CHECK (jsonb_typeof(allow_rules) = 'array'),
    CONSTRAINT dependency_update_configs_ignore_rules_array CHECK (jsonb_typeof(ignore_rules) = 'array'),
    CONSTRAINT dependency_update_configs_groups_object CHECK (jsonb_typeof(groups) = 'object'),
    CONSTRAINT dependency_update_configs_registries_array CHECK (jsonb_typeof(registries) = 'array'),
    CONSTRAINT dependency_update_configs_hash_shape CHECK (char_length(raw_config_hash) BETWEEN 1 AND 128),
    CONSTRAINT dependency_update_configs_path_shape CHECK (char_length(raw_config_path) BETWEEN 1 AND 1024),
    CONSTRAINT dependency_update_configs_last_synced_sha_shape CHECK (char_length(last_synced_sha) <= 128),
    CONSTRAINT dependency_update_configs_unique UNIQUE (repo_id, ecosystem, directory)
);

CREATE INDEX dependency_update_configs_repo_enabled_idx
    ON dependency_update_configs (repo_id, enabled, ecosystem, directory);

CREATE INDEX dependency_update_configs_due_idx
    ON dependency_update_configs (next_run_at, id)
    WHERE enabled = true AND next_run_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_update_configs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE dependency_update_jobs (
    id             bigserial   PRIMARY KEY,
    repo_id        bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    config_id      bigint      NULL REFERENCES dependency_update_configs(id) ON DELETE SET NULL,
    job_kind       text        NOT NULL,
    status         text        NOT NULL DEFAULT 'queued',
    trigger_source text        NOT NULL DEFAULT '',
    scheduled_for  timestamptz NULL,
    started_at     timestamptz NULL,
    completed_at   timestamptz NULL,
    base_sha       text        NOT NULL DEFAULT '',
    head_sha       text        NOT NULL DEFAULT '',
    result_summary jsonb       NOT NULL DEFAULT '{}'::jsonb,
    last_error     text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_update_jobs_kind_known CHECK (job_kind IN ('config_sync', 'security_update', 'version_update', 'triage')),
    CONSTRAINT dependency_update_jobs_status_known CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
    CONSTRAINT dependency_update_jobs_trigger_source_shape CHECK (char_length(trigger_source) <= 80),
    CONSTRAINT dependency_update_jobs_sha_shape CHECK (char_length(base_sha) <= 128 AND char_length(head_sha) <= 128),
    CONSTRAINT dependency_update_jobs_result_summary_object CHECK (jsonb_typeof(result_summary) = 'object'),
    CONSTRAINT dependency_update_jobs_last_error_shape CHECK (char_length(last_error) <= 2000)
);

CREATE INDEX dependency_update_jobs_repo_status_idx
    ON dependency_update_jobs (repo_id, status, created_at DESC);

CREATE INDEX dependency_update_jobs_config_status_idx
    ON dependency_update_jobs (config_id, status, created_at DESC)
    WHERE config_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_update_jobs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE dependency_update_prs (
    id              bigserial   PRIMARY KEY,
    job_id          bigint      NULL REFERENCES dependency_update_jobs(id) ON DELETE SET NULL,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    pull_request_id bigint      NULL REFERENCES pull_requests(issue_id) ON DELETE SET NULL,
    branch_name     text        NOT NULL,
    package_set     jsonb       NOT NULL DEFAULT '[]'::jsonb,
    update_kind     text        NOT NULL,
    status          text        NOT NULL DEFAULT 'open',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_update_prs_branch_name_shape CHECK (char_length(branch_name) BETWEEN 1 AND 1024),
    CONSTRAINT dependency_update_prs_package_set_array CHECK (jsonb_typeof(package_set) = 'array'),
    CONSTRAINT dependency_update_prs_update_kind_known CHECK (update_kind IN ('security', 'version', 'grouped')),
    CONSTRAINT dependency_update_prs_status_known CHECK (status IN ('open', 'merged', 'closed', 'superseded')),
    CONSTRAINT dependency_update_prs_branch_unique UNIQUE (repo_id, branch_name)
);

CREATE INDEX dependency_update_prs_repo_status_idx
    ON dependency_update_prs (repo_id, status, updated_at DESC);

CREATE INDEX dependency_update_prs_pull_request_idx
    ON dependency_update_prs (pull_request_id)
    WHERE pull_request_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_update_prs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE dependency_auto_triage_rules (
    id               bigserial   PRIMARY KEY,
    org_id           bigint      NULL REFERENCES orgs(id) ON DELETE CASCADE,
    repo_id          bigint      NULL REFERENCES repos(id) ON DELETE CASCADE,
    name             text        NOT NULL,
    enabled          boolean     NOT NULL DEFAULT true,
    priority         int         NOT NULL DEFAULT 100,
    match_conditions jsonb       NOT NULL DEFAULT '{}'::jsonb,
    actions          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_by       bigint      NULL REFERENCES users(id) ON DELETE SET NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_auto_triage_rules_scope_exactly_one CHECK ((org_id IS NULL) <> (repo_id IS NULL)),
    CONSTRAINT dependency_auto_triage_rules_name_shape CHECK (char_length(name) BETWEEN 1 AND 120),
    CONSTRAINT dependency_auto_triage_rules_priority_nonnegative CHECK (priority >= 0),
    CONSTRAINT dependency_auto_triage_rules_match_conditions_object CHECK (jsonb_typeof(match_conditions) = 'object'),
    CONSTRAINT dependency_auto_triage_rules_actions_object CHECK (jsonb_typeof(actions) = 'object')
);

CREATE INDEX dependency_auto_triage_rules_org_enabled_idx
    ON dependency_auto_triage_rules (org_id, enabled, priority, id)
    WHERE org_id IS NOT NULL;

CREATE INDEX dependency_auto_triage_rules_repo_enabled_idx
    ON dependency_auto_triage_rules (repo_id, enabled, priority, id)
    WHERE repo_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON dependency_auto_triage_rules
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE dependency_auto_triage_events (
    id          bigserial   PRIMARY KEY,
    rule_id     bigint      NULL REFERENCES dependency_auto_triage_rules(id) ON DELETE SET NULL,
    repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    alert_id    bigint      NOT NULL REFERENCES repo_dependency_alerts(id) ON DELETE CASCADE,
    action      text        NOT NULL,
    outcome     text        NOT NULL,
    message     text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT dependency_auto_triage_events_action_shape CHECK (char_length(action) BETWEEN 1 AND 80),
    CONSTRAINT dependency_auto_triage_events_outcome_known CHECK (outcome IN ('applied', 'skipped', 'failed')),
    CONSTRAINT dependency_auto_triage_events_message_shape CHECK (char_length(message) <= 1000)
);

CREATE INDEX dependency_auto_triage_events_alert_idx
    ON dependency_auto_triage_events (alert_id, created_at DESC);

CREATE INDEX dependency_auto_triage_events_repo_idx
    ON dependency_auto_triage_events (repo_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS dependency_auto_triage_events;
DROP TABLE IF EXISTS dependency_auto_triage_rules;
DROP TABLE IF EXISTS dependency_update_prs;
DROP TABLE IF EXISTS dependency_update_jobs;
DROP TABLE IF EXISTS dependency_update_configs;
