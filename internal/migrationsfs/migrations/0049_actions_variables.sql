-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a actions variables — non-secret per-repo or per-org config.
--
-- Mirrors GHA's `vars` namespace (and Forgejo's actions_variables).
-- Distinct from workflow_secrets because:
--   - Plaintext (no encryption needed; not sensitive)
--   - Surfaced in workflow expressions as ${{ vars.NAME }} (vs
--     ${{ secrets.NAME }})
--   - NOT scrubbed from logs
--
-- Use cases: target image tags, environment names, feature flags,
-- non-secret API endpoints. Operators set these via the same settings
-- pages as secrets (S41c) but without the encryption ceremony.
--
-- Owner XOR + per-scope name uniqueness is identical to workflow_secrets
-- (0045) so the orchestration layer can treat them symmetrically.

-- +goose Up

CREATE TABLE actions_variables (
    id                bigserial    PRIMARY KEY,
    repo_id           bigint       REFERENCES repos(id) ON DELETE CASCADE,
    org_id            bigint       REFERENCES orgs(id)  ON DELETE CASCADE,
    name              citext       NOT NULL,
    value             text         NOT NULL DEFAULT '',
    created_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    updated_at        timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT actions_variables_owner_xor CHECK (
        (repo_id IS NOT NULL AND org_id IS NULL) OR
        (repo_id IS NULL     AND org_id IS NOT NULL)
    ),
    CONSTRAINT actions_variables_name_length CHECK (char_length(name::text) BETWEEN 1 AND 100),
    CONSTRAINT actions_variables_name_format CHECK (name::text ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    CONSTRAINT actions_variables_value_length CHECK (char_length(value) <= 4096)
);

CREATE UNIQUE INDEX actions_variables_repo_name_idx
    ON actions_variables (repo_id, name) WHERE repo_id IS NOT NULL;
CREATE UNIQUE INDEX actions_variables_org_name_idx
    ON actions_variables (org_id, name)  WHERE org_id  IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON actions_variables
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


-- +goose Down
DROP TABLE IF EXISTS actions_variables;
