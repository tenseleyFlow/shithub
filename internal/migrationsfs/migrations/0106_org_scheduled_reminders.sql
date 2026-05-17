-- +goose Up
-- SPDX-License-Identifier: AGPL-3.0-or-later
-- SP20: org scheduled reminders for pull-request review follow-through.

CREATE TYPE org_scheduled_reminder_target AS ENUM ('all_repositories', 'repository', 'team');
CREATE TYPE org_scheduled_reminder_status AS ENUM ('pending', 'sent', 'skipped', 'error');

CREATE TABLE org_scheduled_reminders (
    id                              bigserial PRIMARY KEY,
    org_id                          bigint NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    name                            text NOT NULL,
    target                          org_scheduled_reminder_target NOT NULL DEFAULT 'all_repositories',
    repo_id                         bigint REFERENCES repos(id) ON DELETE CASCADE,
    team_id                         bigint REFERENCES teams(id) ON DELETE CASCADE,
    cron_expr                       text NOT NULL,
    timezone                        text NOT NULL DEFAULT 'UTC',
    next_run_at                     timestamptz NOT NULL,
    last_run_at                     timestamptz,
    last_run_status                 org_scheduled_reminder_status NOT NULL DEFAULT 'pending',
    last_run_error                  text,
    condition_review_requested      boolean NOT NULL DEFAULT true,
    condition_team_review_requested boolean NOT NULL DEFAULT true,
    min_age_minutes                 integer NOT NULL DEFAULT 0,
    paused_at                       timestamptz,
    created_by_user_id              bigint REFERENCES users(id) ON DELETE SET NULL,
    created_at                      timestamptz NOT NULL DEFAULT now(),
    updated_at                      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT org_scheduled_reminders_name_len CHECK (char_length(name) BETWEEN 1 AND 80),
    CONSTRAINT org_scheduled_reminders_cron_len CHECK (char_length(cron_expr) BETWEEN 9 AND 120),
    CONSTRAINT org_scheduled_reminders_tz_len CHECK (char_length(timezone) BETWEEN 1 AND 64),
    CONSTRAINT org_scheduled_reminders_age_nonneg CHECK (min_age_minutes BETWEEN 0 AND 43200),
    CONSTRAINT org_scheduled_reminders_some_condition CHECK (
        condition_review_requested OR condition_team_review_requested
    ),
    CONSTRAINT org_scheduled_reminders_target_shape CHECK (
        (target = 'all_repositories' AND repo_id IS NULL AND team_id IS NULL)
        OR (target = 'repository' AND repo_id IS NOT NULL AND team_id IS NULL)
        OR (target = 'team' AND repo_id IS NULL AND team_id IS NOT NULL)
    )
);

CREATE INDEX org_scheduled_reminders_org_idx
    ON org_scheduled_reminders (org_id, created_at DESC);
CREATE INDEX org_scheduled_reminders_due_idx
    ON org_scheduled_reminders (next_run_at)
    WHERE paused_at IS NULL;
CREATE INDEX org_scheduled_reminders_repo_idx
    ON org_scheduled_reminders (repo_id)
    WHERE repo_id IS NOT NULL;
CREATE INDEX org_scheduled_reminders_team_idx
    ON org_scheduled_reminders (team_id)
    WHERE team_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON org_scheduled_reminders
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE org_scheduled_reminder_deliveries (
    schedule_id       bigint NOT NULL REFERENCES org_scheduled_reminders(id) ON DELETE CASCADE,
    run_key           timestamptz NOT NULL,
    pr_issue_id       bigint NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    recipient_user_id bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notification_id   bigint REFERENCES notifications(id) ON DELETE SET NULL,
    delivered_at      timestamptz,

    PRIMARY KEY (schedule_id, run_key, pr_issue_id, recipient_user_id)
);

CREATE INDEX org_scheduled_reminder_deliveries_recipient_idx
    ON org_scheduled_reminder_deliveries (recipient_user_id, delivered_at DESC);

-- +goose Down
DROP TABLE IF EXISTS org_scheduled_reminder_deliveries;
DROP TABLE IF EXISTS org_scheduled_reminders;
DROP TYPE IF EXISTS org_scheduled_reminder_status;
DROP TYPE IF EXISTS org_scheduled_reminder_target;
