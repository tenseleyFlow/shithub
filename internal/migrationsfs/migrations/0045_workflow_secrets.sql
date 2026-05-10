-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S41a workflow secrets — per-repo or per-org encrypted secrets the
-- workflow engine surfaces to runners as env vars on the job container.
--
-- Encryption uses internal/auth/secretbox (ChaCha20Poly1305, AEAD)
-- with the master key from cfg.Auth.TOTPKeyB64 — same trust domain as
-- webhook secrets (S33). The (ciphertext, nonce) pair lives here;
-- plaintext is never stored.
--
-- Owner is XOR: exactly one of repo_id / org_id is non-NULL. The
-- check constraint enforces; partial unique indexes give scope-local
-- name uniqueness without a separate composite key.
--
-- Names are case-insensitive (citext) so `MY_SECRET` and `my_secret`
-- collide. Mirrors GHA semantics where secrets are uppercased by
-- convention but the lookup is case-insensitive.
--
-- Wired by S41c: CRUD handlers under settings/secrets/actions, plus
-- the runner-API surface that resolves secret bindings into the
-- per-job env on dispatch.

-- +goose Up

CREATE TABLE workflow_secrets (
    id                bigserial    PRIMARY KEY,
    repo_id           bigint       REFERENCES repos(id) ON DELETE CASCADE,
    org_id            bigint       REFERENCES orgs(id)  ON DELETE CASCADE,
    name              citext       NOT NULL,
    ciphertext        bytea        NOT NULL,
    nonce             bytea        NOT NULL,
    created_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    created_at        timestamptz  NOT NULL DEFAULT now(),
    updated_at        timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT workflow_secrets_owner_xor CHECK (
        (repo_id IS NOT NULL AND org_id IS NULL) OR
        (repo_id IS NULL     AND org_id IS NOT NULL)
    ),
    CONSTRAINT workflow_secrets_name_length CHECK (char_length(name::text) BETWEEN 1 AND 100),
    CONSTRAINT workflow_secrets_name_format CHECK (name::text ~ '^[A-Za-z_][A-Za-z0-9_]*$'),
    CONSTRAINT workflow_secrets_nonce_length CHECK (octet_length(nonce) = 12),
    CONSTRAINT workflow_secrets_ciphertext_nonempty CHECK (octet_length(ciphertext) > 0)
);

CREATE UNIQUE INDEX workflow_secrets_repo_name_idx
    ON workflow_secrets (repo_id, name) WHERE repo_id IS NOT NULL;
CREATE UNIQUE INDEX workflow_secrets_org_name_idx
    ON workflow_secrets (org_id, name)  WHERE org_id  IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON workflow_secrets
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


-- +goose Down
DROP TABLE IF EXISTS workflow_secrets;
