-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-12: personal Actions secrets + variables.
--
-- Extends workflow_secrets and actions_variables with a third scope:
-- user_id. A user-scoped secret/variable is visible to every workflow
-- run in a repo that user owns. Mirrors org-scope semantics for user-
-- owned repos.
--
-- Resolution order at job dispatch (PRO-EXT01-12b will wire this):
--   repo  → user/org  → unset
--
-- The XOR constraint is relaxed from (repo, org) to (repo, org, user).
-- ON DELETE CASCADE on user_id mirrors the existing repo/org policy:
-- when the principal disappears, its secrets do too. The deletion is
-- destructive — no soft-delete grace — same as before.
--
-- Security notes:
--   * Existing ciphertext column is unchanged; the AEAD layer in
--     internal/auth/secretbox keeps owning the format.
--   * The new partial-unique index for (user_id, name) mirrors the
--     existing (repo_id, name) + (org_id, name) indices.
--   * Variables remain plaintext (4096 char cap from the parent
--     migration); no schema change beyond the new owner column.
--
-- +goose Up
ALTER TABLE workflow_secrets
    ADD COLUMN user_id bigint REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE workflow_secrets DROP CONSTRAINT workflow_secrets_owner_xor;
ALTER TABLE workflow_secrets ADD CONSTRAINT workflow_secrets_owner_xor CHECK (
    (repo_id IS NOT NULL AND org_id IS NULL     AND user_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NOT NULL AND user_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NULL     AND user_id IS NOT NULL)
);

CREATE UNIQUE INDEX workflow_secrets_user_name_idx
    ON workflow_secrets (user_id, name) WHERE user_id IS NOT NULL;

ALTER TABLE actions_variables
    ADD COLUMN user_id bigint REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE actions_variables DROP CONSTRAINT actions_variables_owner_xor;
ALTER TABLE actions_variables ADD CONSTRAINT actions_variables_owner_xor CHECK (
    (repo_id IS NOT NULL AND org_id IS NULL     AND user_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NOT NULL AND user_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NULL     AND user_id IS NOT NULL)
);

CREATE UNIQUE INDEX actions_variables_user_name_idx
    ON actions_variables (user_id, name) WHERE user_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS actions_variables_user_name_idx;
ALTER TABLE actions_variables DROP CONSTRAINT actions_variables_owner_xor;
ALTER TABLE actions_variables ADD CONSTRAINT actions_variables_owner_xor CHECK (
    (repo_id IS NOT NULL AND org_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NOT NULL)
);
ALTER TABLE actions_variables DROP COLUMN user_id;

DROP INDEX IF EXISTS workflow_secrets_user_name_idx;
ALTER TABLE workflow_secrets DROP CONSTRAINT workflow_secrets_owner_xor;
ALTER TABLE workflow_secrets ADD CONSTRAINT workflow_secrets_owner_xor CHECK (
    (repo_id IS NOT NULL AND org_id IS NULL) OR
    (repo_id IS NULL     AND org_id IS NOT NULL)
);
ALTER TABLE workflow_secrets DROP COLUMN user_id;
