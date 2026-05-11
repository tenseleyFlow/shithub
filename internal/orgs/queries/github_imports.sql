-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: CreateOrgGithubImport :one
INSERT INTO org_github_imports (
    org_id, source_org, requested_by_user_id, include_private,
    token_present, token_ciphertext, token_nonce
) VALUES (
    $1, $2, sqlc.narg(requested_by_user_id)::bigint, $3,
    $4, sqlc.narg(token_ciphertext)::bytea, sqlc.narg(token_nonce)::bytea
)
RETURNING *;

-- name: GetOrgGithubImport :one
SELECT * FROM org_github_imports WHERE id = $1;

-- name: GetOrgGithubImportForOrg :one
SELECT * FROM org_github_imports
WHERE id = $1 AND org_id = $2;

-- name: ListOrgGithubImportsForOrg :many
SELECT * FROM org_github_imports
WHERE org_id = $1
ORDER BY created_at DESC
LIMIT $2;

-- name: MarkOrgGithubImportDiscovering :exec
UPDATE org_github_imports
   SET status = 'discovering',
       started_at = COALESCE(started_at, now()),
       last_error = NULL,
       updated_at = now()
 WHERE id = $1
   AND status IN ('queued', 'discovering');

-- name: MarkOrgGithubImportImporting :exec
UPDATE org_github_imports
   SET status = 'importing',
       total_count = $2,
       started_at = COALESCE(started_at, now()),
       last_error = NULL,
       updated_at = now()
 WHERE id = $1
   AND status IN ('queued', 'discovering', 'importing');

-- name: MarkOrgGithubImportFailed :exec
UPDATE org_github_imports
   SET status = 'failed',
       last_error = $2,
       token_ciphertext = NULL,
       token_nonce = NULL,
       completed_at = COALESCE(completed_at, now()),
       updated_at = now()
 WHERE id = $1;

-- name: MarkOrgGithubImportCompleted :exec
UPDATE org_github_imports
   SET status = 'completed',
       token_ciphertext = NULL,
       token_nonce = NULL,
       completed_at = COALESCE(completed_at, now()),
       updated_at = now()
 WHERE id = $1;

-- name: MarkOrgGithubImportCompletedIfDone :one
UPDATE org_github_imports AS i
   SET status = 'completed',
       token_ciphertext = NULL,
       token_nonce = NULL,
       completed_at = COALESCE(completed_at, now()),
       updated_at = now()
 WHERE i.id = $1
   AND i.status = 'importing'
   AND NOT EXISTS (
       SELECT 1
         FROM org_github_import_repos
        WHERE import_id = $1
          AND status IN ('queued', 'importing')
   )
RETURNING i.*;

-- name: InsertOrgGithubImportRepo :one
INSERT INTO org_github_import_repos (
    import_id, github_id, source_full_name, source_name, target_name,
    clone_url, description, default_branch, target_visibility,
    is_private, is_fork
) VALUES (
    $1, sqlc.narg(github_id)::bigint, $2, $3, $4,
    $5, $6, $7, $8, $9, $10
)
ON CONFLICT (import_id, target_name) DO UPDATE
   SET github_id = EXCLUDED.github_id,
       source_full_name = EXCLUDED.source_full_name,
       source_name = EXCLUDED.source_name,
       clone_url = EXCLUDED.clone_url,
       description = EXCLUDED.description,
       default_branch = EXCLUDED.default_branch,
       target_visibility = EXCLUDED.target_visibility,
       is_private = EXCLUDED.is_private,
       is_fork = EXCLUDED.is_fork,
       updated_at = now()
RETURNING *;

-- name: GetOrgGithubImportRepo :one
SELECT * FROM org_github_import_repos WHERE id = $1;

-- name: ListOrgGithubImportRepos :many
SELECT * FROM org_github_import_repos
WHERE import_id = $1
ORDER BY source_name ASC;

-- name: MarkOrgGithubImportRepoImporting :exec
UPDATE org_github_import_repos
   SET status = 'importing',
       started_at = COALESCE(started_at, now()),
       last_error = NULL,
       updated_at = now()
 WHERE id = $1
   AND status = 'queued';

-- name: MarkOrgGithubImportRepoImported :exec
UPDATE org_github_import_repos
   SET status = 'imported',
       repo_id = $2,
       last_error = NULL,
       completed_at = COALESCE(completed_at, now()),
       updated_at = now()
 WHERE id = $1;

-- name: MarkOrgGithubImportRepoSkipped :exec
UPDATE org_github_import_repos
   SET status = 'skipped',
       last_error = $2,
       completed_at = COALESCE(completed_at, now()),
       updated_at = now()
 WHERE id = $1;

-- name: MarkOrgGithubImportRepoFailed :exec
UPDATE org_github_import_repos
   SET status = 'failed',
       repo_id = COALESCE(sqlc.narg(repo_id)::bigint, repo_id),
       last_error = $2,
       completed_at = COALESCE(completed_at, now()),
       updated_at = now()
 WHERE id = $1;

-- name: GetOrgGithubImportProgress :one
SELECT
    i.*,
    count(r.id)::integer AS discovered_count,
    count(r.id) FILTER (WHERE r.status = 'queued')::integer AS queued_count,
    count(r.id) FILTER (WHERE r.status = 'importing')::integer AS importing_count,
    count(r.id) FILTER (WHERE r.status = 'imported')::integer AS imported_count,
    count(r.id) FILTER (WHERE r.status = 'skipped')::integer AS skipped_count,
    count(r.id) FILTER (WHERE r.status = 'failed')::integer AS failed_count
FROM org_github_imports i
LEFT JOIN org_github_import_repos r ON r.import_id = i.id
WHERE i.id = $1 AND i.org_id = $2
GROUP BY i.id;
