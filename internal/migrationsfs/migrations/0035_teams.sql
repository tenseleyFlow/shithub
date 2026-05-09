-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S31 — Teams within organizations.
--
-- One level of nesting is intentional and structurally enforced via
-- two CHECKs:
--   - parent_team_id != id  (no self-parent)
--   - if parent_team_id IS NOT NULL then the parent's parent_team_id
--     is NULL (validated via a trigger; can't express in a row CHECK)
--
-- team_repo_access is the per-team repo grant; the per-user grant
-- continues to live in `repo_collaborators` (S15). Both contribute to
-- the policy aggregator's max-of-sources rule.

-- +goose Up

CREATE TYPE team_privacy   AS ENUM ('visible', 'secret');
CREATE TYPE team_role      AS ENUM ('member', 'maintainer');
CREATE TYPE team_repo_role AS ENUM ('read', 'triage', 'write', 'maintain', 'admin');

CREATE TABLE teams (
    id                 bigserial    PRIMARY KEY,
    org_id             bigint       NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    slug               citext       NOT NULL,
    display_name       text         NOT NULL DEFAULT '',
    description        text         NOT NULL DEFAULT '',
    parent_team_id     bigint       REFERENCES teams(id) ON DELETE SET NULL,
    privacy            team_privacy NOT NULL DEFAULT 'visible',
    created_by_user_id bigint       REFERENCES users(id) ON DELETE SET NULL,
    created_at         timestamptz  NOT NULL DEFAULT now(),
    updated_at         timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT teams_slug_length CHECK (char_length(slug::text) BETWEEN 1 AND 50),
    CONSTRAINT teams_no_self_parent CHECK (parent_team_id IS NULL OR parent_team_id <> id),
    CONSTRAINT teams_org_slug_unique UNIQUE (org_id, slug)
);

CREATE INDEX teams_org_idx ON teams (org_id);
CREATE INDEX teams_parent_idx ON teams (parent_team_id) WHERE parent_team_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON teams
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- One-level-deep nesting enforcement: a team's parent must itself
-- have a NULL parent. Implemented as a trigger because the rule
-- spans rows.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_teams_one_level_nesting() RETURNS trigger AS $$
DECLARE
    grandparent_id bigint;
BEGIN
    IF NEW.parent_team_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT parent_team_id INTO grandparent_id FROM teams WHERE id = NEW.parent_team_id;
    IF grandparent_id IS NOT NULL THEN
        RAISE EXCEPTION 'teams: nesting limited to one level (parent already has a parent)'
            USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER tg_teams_one_level_nesting_iu
    BEFORE INSERT OR UPDATE ON teams
    FOR EACH ROW EXECUTE FUNCTION tg_teams_one_level_nesting();

-- ─── team_members ─────────────────────────────────────────────────
CREATE TABLE team_members (
    team_id          bigint      NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id          bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role             team_role   NOT NULL DEFAULT 'member',
    added_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    added_at         timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (team_id, user_id)
);

CREATE INDEX team_members_user_idx ON team_members (user_id);

-- ─── team_repo_access ─────────────────────────────────────────────
CREATE TABLE team_repo_access (
    team_id          bigint         NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    repo_id          bigint         NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    role             team_repo_role NOT NULL DEFAULT 'read',
    added_by_user_id bigint         REFERENCES users(id) ON DELETE SET NULL,
    added_at         timestamptz    NOT NULL DEFAULT now(),

    PRIMARY KEY (team_id, repo_id)
);

CREATE INDEX team_repo_access_repo_idx ON team_repo_access (repo_id);

-- +goose Down
DROP TABLE IF EXISTS team_repo_access;
DROP TABLE IF EXISTS team_members;
DROP TRIGGER IF EXISTS tg_teams_one_level_nesting_iu ON teams;
DROP FUNCTION IF EXISTS tg_teams_one_level_nesting();
DROP TABLE IF EXISTS teams;
DROP TYPE IF EXISTS team_repo_role;
DROP TYPE IF EXISTS team_role;
DROP TYPE IF EXISTS team_privacy;
