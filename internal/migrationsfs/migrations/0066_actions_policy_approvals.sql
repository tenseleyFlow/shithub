-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41j-3 Actions policy, abuse caps, and approval decisions.
--
-- The existing workflow_runs.need_approval / approved_by_user_id columns remain
-- the fast path for runner dispatch. The policy tables below hold durable
-- site/org/repo defaults, and workflow_run_approvals records the explicit
-- approval/rejection decision without overloading run status fields.

-- +goose Up

CREATE TYPE actions_policy_state AS ENUM ('inherit', 'enabled', 'disabled');

CREATE TABLE actions_site_policy (
    id                                boolean PRIMARY KEY DEFAULT true CHECK (id),
    actions_enabled                   boolean NOT NULL DEFAULT true,
    require_pr_approval               boolean NOT NULL DEFAULT true,
    max_repo_queued_runs              integer NOT NULL DEFAULT 50,
    max_repo_concurrent_jobs          integer NOT NULL DEFAULT 20,
    max_owner_concurrent_jobs         integer NOT NULL DEFAULT 100,
    actor_trigger_limit_per_hour      integer NOT NULL DEFAULT 120,
    updated_by_user_id                bigint REFERENCES users(id) ON DELETE SET NULL,
    created_at                        timestamptz NOT NULL DEFAULT now(),
    updated_at                        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT actions_site_policy_caps_nonnegative CHECK (
        max_repo_queued_runs >= 0
        AND max_repo_concurrent_jobs >= 0
        AND max_owner_concurrent_jobs >= 0
        AND actor_trigger_limit_per_hour >= 0
    )
);

INSERT INTO actions_site_policy (id) VALUES (true);

CREATE TABLE actions_org_policies (
    org_id                            bigint PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    actions_enabled                   actions_policy_state NOT NULL DEFAULT 'inherit',
    require_pr_approval               boolean,
    max_repo_queued_runs              integer,
    max_repo_concurrent_jobs          integer,
    max_owner_concurrent_jobs         integer,
    actor_trigger_limit_per_hour      integer,
    updated_by_user_id                bigint REFERENCES users(id) ON DELETE SET NULL,
    created_at                        timestamptz NOT NULL DEFAULT now(),
    updated_at                        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT actions_org_policies_caps_nonnegative CHECK (
        (max_repo_queued_runs IS NULL OR max_repo_queued_runs >= 0)
        AND (max_repo_concurrent_jobs IS NULL OR max_repo_concurrent_jobs >= 0)
        AND (max_owner_concurrent_jobs IS NULL OR max_owner_concurrent_jobs >= 0)
        AND (actor_trigger_limit_per_hour IS NULL OR actor_trigger_limit_per_hour >= 0)
    )
);

CREATE TABLE actions_repo_policies (
    repo_id                           bigint PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    actions_enabled                   actions_policy_state NOT NULL DEFAULT 'inherit',
    require_pr_approval               boolean,
    max_repo_queued_runs              integer,
    max_repo_concurrent_jobs          integer,
    max_owner_concurrent_jobs         integer,
    actor_trigger_limit_per_hour      integer,
    updated_by_user_id                bigint REFERENCES users(id) ON DELETE SET NULL,
    created_at                        timestamptz NOT NULL DEFAULT now(),
    updated_at                        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT actions_repo_policies_caps_nonnegative CHECK (
        (max_repo_queued_runs IS NULL OR max_repo_queued_runs >= 0)
        AND (max_repo_concurrent_jobs IS NULL OR max_repo_concurrent_jobs >= 0)
        AND (max_owner_concurrent_jobs IS NULL OR max_owner_concurrent_jobs >= 0)
        AND (actor_trigger_limit_per_hour IS NULL OR actor_trigger_limit_per_hour >= 0)
    )
);

CREATE TABLE workflow_run_approvals (
    run_id              bigint PRIMARY KEY REFERENCES workflow_runs(id) ON DELETE CASCADE,
    requested_reason    text NOT NULL DEFAULT '',
    requested_at        timestamptz NOT NULL DEFAULT now(),
    approved_by_user_id bigint REFERENCES users(id) ON DELETE SET NULL,
    approved_at         timestamptz,
    rejected_by_user_id bigint REFERENCES users(id) ON DELETE SET NULL,
    rejected_at         timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workflow_run_approvals_one_terminal_decision CHECK (
        NOT (approved_at IS NOT NULL AND rejected_at IS NOT NULL)
    ),
    CONSTRAINT workflow_run_approvals_approved_actor CHECK (
        (approved_at IS NULL AND approved_by_user_id IS NULL)
        OR (approved_at IS NOT NULL AND approved_by_user_id IS NOT NULL)
    ),
    CONSTRAINT workflow_run_approvals_rejected_actor CHECK (
        (rejected_at IS NULL AND rejected_by_user_id IS NULL)
        OR (rejected_at IS NOT NULL AND rejected_by_user_id IS NOT NULL)
    )
);

CREATE INDEX workflow_run_approvals_pending_idx
    ON workflow_run_approvals (requested_at)
    WHERE approved_at IS NULL AND rejected_at IS NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON actions_site_policy
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON actions_org_policies
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON actions_repo_policies
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow_run_approvals
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS workflow_run_approvals;
DROP TABLE IF EXISTS actions_repo_policies;
DROP TABLE IF EXISTS actions_org_policies;
DROP TABLE IF EXISTS actions_site_policy;
DROP TYPE IF EXISTS actions_policy_state;
