-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: UpsertRepoSecret :one
INSERT INTO workflow_secrets (repo_id, name, ciphertext, nonce, created_by_user_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (repo_id, name) WHERE repo_id IS NOT NULL DO UPDATE
SET ciphertext = EXCLUDED.ciphertext,
    nonce      = EXCLUDED.nonce,
    updated_at = now()
RETURNING id, repo_id, org_id, name, ciphertext, nonce,
          created_by_user_id, created_at, updated_at;

-- name: UpsertOrgSecret :one
INSERT INTO workflow_secrets (org_id, name, ciphertext, nonce, created_by_user_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (org_id, name) WHERE org_id IS NOT NULL DO UPDATE
SET ciphertext = EXCLUDED.ciphertext,
    nonce      = EXCLUDED.nonce,
    updated_at = now()
RETURNING id, repo_id, org_id, name, ciphertext, nonce,
          created_by_user_id, created_at, updated_at;

-- name: ListRepoSecrets :many
SELECT id, name, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE repo_id = $1
ORDER BY name ASC;

-- name: ListOrgSecrets :many
SELECT id, name, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE org_id = $1
ORDER BY name ASC;

-- name: GetRepoSecret :one
SELECT id, name, ciphertext, nonce
FROM workflow_secrets
WHERE repo_id = $1 AND name = $2;

-- name: GetOrgSecret :one
SELECT id, name, ciphertext, nonce
FROM workflow_secrets
WHERE org_id = $1 AND name = $2;

-- name: DeleteRepoSecret :exec
DELETE FROM workflow_secrets WHERE repo_id = $1 AND name = $2;

-- name: DeleteOrgSecret :exec
DELETE FROM workflow_secrets WHERE org_id = $1 AND name = $2;

-- PRO-EXT01-12: personal Actions secrets. User-scoped rows mirror the
-- (repo_id, org_id) variants — same encrypted storage, same XOR
-- discriminator (now 3-way), separate partial unique index per scope.

-- name: UpsertUserSecret :one
INSERT INTO workflow_secrets (user_id, name, ciphertext, nonce, created_by_user_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, name) WHERE user_id IS NOT NULL DO UPDATE
SET ciphertext = EXCLUDED.ciphertext,
    nonce      = EXCLUDED.nonce,
    updated_at = now()
RETURNING id, repo_id, org_id, name, ciphertext, nonce,
          created_by_user_id, created_at, updated_at;

-- name: ListUserSecrets :many
SELECT id, name, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE user_id = $1
ORDER BY name ASC;

-- name: GetUserSecret :one
SELECT id, name, ciphertext, nonce
FROM workflow_secrets
WHERE user_id = $1 AND name = $2;

-- name: DeleteUserSecret :exec
DELETE FROM workflow_secrets WHERE user_id = $1 AND name = $2;

-- SP23: environment-scoped rows are repo-environment-local. They are visible
-- only to jobs whose workflow declares the matching environment, and they
-- shadow repo/org/user scope during runner resolution.

-- name: UpsertEnvironmentSecret :one
INSERT INTO workflow_secrets (environment_id, name, ciphertext, nonce, created_by_user_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (environment_id, name) WHERE environment_id IS NOT NULL DO UPDATE
SET ciphertext = EXCLUDED.ciphertext,
    nonce      = EXCLUDED.nonce,
    updated_at = now()
RETURNING id, repo_id, org_id, name, ciphertext, nonce,
          created_by_user_id, created_at, updated_at;

-- name: ListEnvironmentSecrets :many
SELECT id, name, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE environment_id = $1
ORDER BY name ASC;

-- name: GetEnvironmentSecret :one
SELECT id, name, ciphertext, nonce
FROM workflow_secrets
WHERE environment_id = $1 AND name = $2;

-- name: DeleteEnvironmentSecret :exec
DELETE FROM workflow_secrets WHERE environment_id = $1 AND name = $2;

-- PRO-EXT_SR-08: bulk-fetch + meta-only single-row queries to retire
-- two N+1 patterns. The runner's mergeUserSecrets used to do 1 List
-- followed by N GetUserSecret calls; the REST GET-by-name did a list
-- scan instead of a direct lookup.

-- name: ListUserSecretsWithCiphertext :many
SELECT id, name, ciphertext, nonce, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE user_id = $1
ORDER BY name ASC;

-- name: GetUserSecretMeta :one
SELECT id, name, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE user_id = $1 AND name = $2;

-- PRO-EXT_SR2-12 (audit Q1): retire the same 1+N pattern for the
-- repo and org variants that previously haunted mergeUserSecrets.
-- Runners resolving a job that uses N repo or org secrets used to
-- do 1 List + N GetRepoSecret/GetOrgSecret round-trips; this returns
-- ciphertext + nonce in the single list query.

-- name: ListRepoSecretsWithCiphertext :many
SELECT id, name, ciphertext, nonce, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE repo_id = $1
ORDER BY name ASC;

-- name: ListOrgSecretsWithCiphertext :many
SELECT id, name, ciphertext, nonce, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE org_id = $1
ORDER BY name ASC;

-- name: ListEnvironmentSecretsWithCiphertext :many
SELECT id, name, ciphertext, nonce, created_by_user_id, created_at, updated_at
FROM workflow_secrets
WHERE environment_id = $1
ORDER BY name ASC;
