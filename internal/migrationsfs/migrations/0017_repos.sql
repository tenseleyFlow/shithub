-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Repos table:
--
-- - One row per repository. Owner is XOR (user OR org); orgs land in
--   S31 but the column exists from day 1 so the FK isn't a future
--   destructive migration.
-- - Name is citext so case-insensitive uniqueness comes for free.
-- - default_branch is "trunk" by default; we'll let owners change it
--   later. The bare repo on disk is created with the same default
--   (--initial-branch=trunk via storage.RepoFS.InitBare).
-- - Soft-delete + archive flags are introduced now so callers don't
--   have to retrofit them later.
-- - disk_used_bytes is the recorded post-init size; recalc job is S14.
-- - has_issues / has_pulls let owners turn off these surfaces. Default
--   on; the per-repo settings UI lands later.

-- +goose Up
CREATE TYPE repo_visibility AS ENUM ('public', 'private');

CREATE TABLE repos (
    id              bigserial   PRIMARY KEY,
    owner_user_id   bigint      REFERENCES users(id) ON DELETE CASCADE,
    owner_org_id    bigint, -- FK added when orgs ship in S31
    name            citext      NOT NULL,
    description     text        NOT NULL DEFAULT '',
    visibility      repo_visibility NOT NULL DEFAULT 'public',
    default_branch  text        NOT NULL DEFAULT 'trunk',
    is_archived     boolean     NOT NULL DEFAULT false,
    archived_at     timestamptz,
    deleted_at      timestamptz,
    disk_used_bytes bigint      NOT NULL DEFAULT 0,
    fork_of_repo_id bigint      REFERENCES repos(id) ON DELETE SET NULL,
    license_key     text,
    primary_language text,
    has_issues      boolean     NOT NULL DEFAULT true,
    has_pulls       boolean     NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- Exactly one owner kind. Until S31 ships orgs the right side is
    -- always NULL; the constraint shape is forward-compatible.
    CONSTRAINT repos_owner_xor CHECK (
        (owner_user_id IS NOT NULL AND owner_org_id IS NULL)
     OR (owner_user_id IS NULL     AND owner_org_id IS NOT NULL)
    ),
    CONSTRAINT repos_name_length CHECK (char_length(name::text) BETWEEN 1 AND 100),
    CONSTRAINT repos_description_length CHECK (char_length(description) <= 350),
    CONSTRAINT repos_default_branch_length CHECK (char_length(default_branch) BETWEEN 1 AND 100)
);

-- Per-owner uniqueness. Two partial indexes — one per owner kind —
-- so a name can't collide within either bucket.
CREATE UNIQUE INDEX repos_owner_user_name_idx
    ON repos (owner_user_id, name)
    WHERE owner_user_id IS NOT NULL;

CREATE UNIQUE INDEX repos_owner_org_name_idx
    ON repos (owner_org_id, name)
    WHERE owner_org_id IS NOT NULL;

CREATE INDEX repos_visibility_idx ON repos (visibility);
CREATE INDEX repos_deleted_at_idx ON repos (deleted_at) WHERE deleted_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON repos
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS repos;
DROP TYPE IF EXISTS repo_visibility;
