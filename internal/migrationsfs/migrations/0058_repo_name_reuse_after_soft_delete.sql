-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Soft-deleted repos must no longer reserve their owner/name forever.
-- Runtime lifecycle code moves the bare repo to a tombstone path so
-- the canonical on-disk path can be reused by a fresh repo.

-- +goose Up
DROP INDEX IF EXISTS repos_owner_user_name_idx;
DROP INDEX IF EXISTS repos_owner_org_name_idx;

CREATE UNIQUE INDEX repos_owner_user_name_idx
    ON repos (owner_user_id, name)
    WHERE owner_user_id IS NOT NULL AND deleted_at IS NULL;

CREATE UNIQUE INDEX repos_owner_org_name_idx
    ON repos (owner_org_id, name)
    WHERE owner_org_id IS NOT NULL AND deleted_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS repos_owner_user_name_idx;
DROP INDEX IF EXISTS repos_owner_org_name_idx;

-- This rollback can fail if a name has been reused while the prior row
-- remains soft-deleted. That is intentional: reverting the old invariant
-- requires resolving those duplicates first.
CREATE UNIQUE INDEX repos_owner_user_name_idx
    ON repos (owner_user_id, name)
    WHERE owner_user_id IS NOT NULL;

CREATE UNIQUE INDEX repos_owner_org_name_idx
    ON repos (owner_org_id, name)
    WHERE owner_org_id IS NOT NULL;
