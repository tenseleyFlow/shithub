-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S24 PR-checks subsystem.
--
--   check_suites — one row per (repo_id, head_sha, app_slug). Suites
--                  group runs from a single CI app/integration. Status
--                  + conclusion are derived from runs (suite_rollup.go).
--                  app_slug='external' is the default catch-all for
--                  generic CI integrations posting via PAT.
--   check_runs   — individual checks (e.g. "lint", "unit-tests") for
--                  a specific head_sha. Status moves queued → in_progress
--                  → completed; conclusion is set on completion. The
--                  output jsonb mirrors GitHub's {title, summary, text}
--                  shape so existing CI adapters port cleanly.
--
-- Branch protection: status_checks_required already exists from S20
-- (text[]). This migration adds dismiss_stale_status_checks_on_push so
-- the optional stale-on-push behavior can be opted into.

-- +goose Up

CREATE TYPE check_status     AS ENUM ('queued', 'in_progress', 'completed', 'pending');
CREATE TYPE check_conclusion AS ENUM (
    'success', 'failure', 'neutral', 'cancelled',
    'skipped', 'timed_out', 'action_required', 'stale'
);

CREATE TABLE check_suites (
    id          bigserial         PRIMARY KEY,
    repo_id     bigint            NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    head_sha    text              NOT NULL,
    app_slug    text              NOT NULL DEFAULT 'external',
    status      check_status      NOT NULL DEFAULT 'queued',
    conclusion  check_conclusion,
    created_at  timestamptz       NOT NULL DEFAULT now(),
    updated_at  timestamptz       NOT NULL DEFAULT now(),

    UNIQUE (repo_id, head_sha, app_slug),
    CONSTRAINT check_suites_app_slug_length CHECK (char_length(app_slug) BETWEEN 1 AND 64),
    CONSTRAINT check_suites_head_sha_format CHECK (char_length(head_sha) BETWEEN 7 AND 64)
);

CREATE INDEX check_suites_repo_head_idx ON check_suites (repo_id, head_sha);
CREATE INDEX check_suites_status_idx    ON check_suites (repo_id, status);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON check_suites
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


CREATE TABLE check_runs (
    id            bigserial         PRIMARY KEY,
    suite_id      bigint            NOT NULL REFERENCES check_suites(id) ON DELETE CASCADE,
    repo_id       bigint            NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    head_sha      text              NOT NULL,
    name          text              NOT NULL,
    status        check_status      NOT NULL DEFAULT 'queued',
    conclusion    check_conclusion,
    started_at    timestamptz,
    completed_at  timestamptz,
    details_url   text              NOT NULL DEFAULT '',
    output        jsonb             NOT NULL DEFAULT '{}'::jsonb,
    -- external_id lets external systems dedupe POST creates: the same
    -- (repo, head_sha, name, external_id) returns the existing run.
    external_id   text,
    created_at    timestamptz       NOT NULL DEFAULT now(),
    updated_at    timestamptz       NOT NULL DEFAULT now(),

    CONSTRAINT check_runs_name_length CHECK (char_length(name) BETWEEN 1 AND 200),
    CONSTRAINT check_runs_completed_has_conclusion CHECK (
        status <> 'completed' OR conclusion IS NOT NULL
    )
);

CREATE INDEX check_runs_repo_head_idx       ON check_runs (repo_id, head_sha);
CREATE INDEX check_runs_suite_idx           ON check_runs (suite_id);
CREATE INDEX check_runs_external_id_idx     ON check_runs (repo_id, head_sha, name, external_id)
    WHERE external_id IS NOT NULL;
CREATE INDEX check_runs_required_lookup_idx ON check_runs (repo_id, head_sha, name);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON check_runs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


ALTER TABLE branch_protection_rules
    ADD COLUMN dismiss_stale_status_checks_on_push boolean NOT NULL DEFAULT false;


-- +goose Down
ALTER TABLE branch_protection_rules
    DROP COLUMN IF EXISTS dismiss_stale_status_checks_on_push;
DROP TABLE IF EXISTS check_runs;
DROP TABLE IF EXISTS check_suites;
DROP TYPE IF EXISTS check_conclusion;
DROP TYPE IF EXISTS check_status;
