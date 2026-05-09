-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S30 — Organizations.
--
-- Schema overview:
--
--   orgs              — first-class principals that can own repos.
--   org_members       — (org, user) ↔ role; PK on the pair so a user
--                       holds exactly one role per org.
--   org_invitations   — pending invites by username OR email; one of
--                       target_user_id / target_email is set.
--   principals        — single source of truth for /{slug} resolution.
--                       Maintained by triggers on users + orgs so a
--                       slug collision is structurally impossible.
--
-- The `repos.owner_org_id` column already exists from 0017 with the
-- right XOR CHECK constraint; this migration just adds the FK once
-- the orgs table exists.

-- +goose Up

-- ─── orgs ───────────────────────────────────────────────────────────
CREATE TYPE org_plan AS ENUM ('free', 'team', 'enterprise');

CREATE TABLE orgs (
    id                       bigserial   PRIMARY KEY,
    slug                     citext      NOT NULL UNIQUE,
    display_name             text        NOT NULL DEFAULT '',
    description              text        NOT NULL DEFAULT '',
    avatar_object_key        text,
    location                 text        NOT NULL DEFAULT '',
    website                  text        NOT NULL DEFAULT '',
    billing_email            text        NOT NULL DEFAULT '',
    plan                     org_plan    NOT NULL DEFAULT 'free',
    allow_member_repo_create boolean     NOT NULL DEFAULT true,
    created_by_user_id       bigint      REFERENCES users(id) ON DELETE SET NULL,
    suspended_at             timestamptz,
    suspended_reason         text,
    deleted_at               timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT orgs_slug_length CHECK (char_length(slug::text) BETWEEN 1 AND 39),
    CONSTRAINT orgs_description_length CHECK (char_length(description) <= 350)
);

CREATE INDEX orgs_deleted_at_idx ON orgs (deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX orgs_suspended_at_idx ON orgs (suspended_at) WHERE suspended_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON orgs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- ─── org_members ───────────────────────────────────────────────────
CREATE TYPE org_role AS ENUM ('owner', 'member');

CREATE TABLE org_members (
    org_id              bigint      NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id             bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role                org_role    NOT NULL DEFAULT 'member',
    invited_by_user_id  bigint      REFERENCES users(id) ON DELETE SET NULL,
    joined_at           timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (org_id, user_id)
);

CREATE INDEX org_members_user_idx ON org_members (user_id);
CREATE INDEX org_members_role_idx ON org_members (org_id, role);

-- ─── org_invitations ───────────────────────────────────────────────
CREATE TABLE org_invitations (
    id                   bigserial   PRIMARY KEY,
    org_id               bigint      NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    invited_by_user_id   bigint      REFERENCES users(id) ON DELETE SET NULL,
    target_user_id       bigint      REFERENCES users(id) ON DELETE CASCADE,
    target_email         citext,
    role                 org_role    NOT NULL DEFAULT 'member',
    token_hash           bytea       NOT NULL UNIQUE,
    expires_at           timestamptz NOT NULL,
    accepted_at          timestamptz,
    declined_at          timestamptz,
    canceled_at          timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT org_invites_target_xor CHECK (
        (target_user_id IS NOT NULL AND target_email IS NULL)
     OR (target_user_id IS NULL     AND target_email IS NOT NULL)
    )
);

CREATE INDEX org_invites_org_pending_idx
    ON org_invitations (org_id)
    WHERE accepted_at IS NULL AND declined_at IS NULL AND canceled_at IS NULL;

CREATE INDEX org_invites_target_user_idx
    ON org_invitations (target_user_id)
    WHERE target_user_id IS NOT NULL;

CREATE INDEX org_invites_target_email_idx
    ON org_invitations (target_email)
    WHERE target_email IS NOT NULL;

-- ─── principals ────────────────────────────────────────────────────
-- Unifies the /{slug} URL space across users and orgs. Maintained by
-- AFTER triggers on users + orgs so the row is always coherent.
-- One row per (slug, kind, id); slug PK enforces global uniqueness
-- across both tables — a slug collision is structurally impossible.
CREATE TYPE principal_kind AS ENUM ('user', 'org');

CREATE TABLE principals (
    slug citext         PRIMARY KEY,
    kind principal_kind NOT NULL,
    id   bigint         NOT NULL,

    -- Sanity index for "all orgs" / "all users" sweeps.
    CONSTRAINT principals_id_kind_idx UNIQUE (kind, id)
);

-- Backfill from existing users.
INSERT INTO principals (slug, kind, id)
SELECT username, 'user'::principal_kind, id
FROM users
WHERE deleted_at IS NULL
ON CONFLICT (slug) DO NOTHING;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_principals_user_sync() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM principals WHERE kind = 'user' AND id = OLD.id;
        RETURN OLD;
    END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO principals (slug, kind, id)
        VALUES (NEW.username, 'user', NEW.id);
        RETURN NEW;
    END IF;
    -- UPDATE: handle username change + soft-delete flip.
    IF NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL THEN
        DELETE FROM principals WHERE kind = 'user' AND id = NEW.id;
        RETURN NEW;
    END IF;
    IF NEW.deleted_at IS NULL AND OLD.deleted_at IS NOT NULL THEN
        INSERT INTO principals (slug, kind, id)
        VALUES (NEW.username, 'user', NEW.id)
        ON CONFLICT (slug) DO UPDATE SET kind = EXCLUDED.kind, id = EXCLUDED.id;
        RETURN NEW;
    END IF;
    IF NEW.username <> OLD.username THEN
        UPDATE principals SET slug = NEW.username
            WHERE kind = 'user' AND id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_principals_org_sync() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        DELETE FROM principals WHERE kind = 'org' AND id = OLD.id;
        RETURN OLD;
    END IF;
    IF TG_OP = 'INSERT' THEN
        INSERT INTO principals (slug, kind, id)
        VALUES (NEW.slug, 'org', NEW.id);
        RETURN NEW;
    END IF;
    IF NEW.deleted_at IS NOT NULL AND OLD.deleted_at IS NULL THEN
        DELETE FROM principals WHERE kind = 'org' AND id = NEW.id;
        RETURN NEW;
    END IF;
    IF NEW.deleted_at IS NULL AND OLD.deleted_at IS NOT NULL THEN
        INSERT INTO principals (slug, kind, id)
        VALUES (NEW.slug, 'org', NEW.id)
        ON CONFLICT (slug) DO UPDATE SET kind = EXCLUDED.kind, id = EXCLUDED.id;
        RETURN NEW;
    END IF;
    IF NEW.slug <> OLD.slug THEN
        UPDATE principals SET slug = NEW.slug
            WHERE kind = 'org' AND id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER tg_principals_user_sync_iud
    AFTER INSERT OR UPDATE OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION tg_principals_user_sync();

CREATE TRIGGER tg_principals_org_sync_iud
    AFTER INSERT OR UPDATE OR DELETE ON orgs
    FOR EACH ROW EXECUTE FUNCTION tg_principals_org_sync();

-- ─── repos.owner_org_id FK ─────────────────────────────────────────
-- The column existed from 0017 with the XOR CHECK already in place;
-- adding the actual FK only now that the target table exists.
ALTER TABLE repos
    ADD CONSTRAINT repos_owner_org_id_fkey
        FOREIGN KEY (owner_org_id) REFERENCES orgs(id) ON DELETE CASCADE;

-- +goose Down
ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_owner_org_id_fkey;
DROP TRIGGER IF EXISTS tg_principals_org_sync_iud ON orgs;
DROP TRIGGER IF EXISTS tg_principals_user_sync_iud ON users;
DROP FUNCTION IF EXISTS tg_principals_org_sync();
DROP FUNCTION IF EXISTS tg_principals_user_sync();
DROP TABLE IF EXISTS principals;
DROP TYPE IF EXISTS principal_kind;
DROP TABLE IF EXISTS org_invitations;
DROP TABLE IF EXISTS org_members;
DROP TYPE IF EXISTS org_role;
DROP TABLE IF EXISTS orgs;
DROP TYPE IF EXISTS org_plan;
