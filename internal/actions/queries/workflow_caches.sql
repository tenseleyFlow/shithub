-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertWorkflowCache :one
-- Called by the future runner-side upload handler when an
-- actions/cache step completes its tarball upload. Idempotent on
-- (repo_id, cache_key, cache_version, git_ref): a re-upload with the
-- same coordinates updates size + last_accessed_at + object_key.
INSERT INTO workflow_caches (
    repo_id, cache_key, cache_version, git_ref, object_key, size_bytes
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (repo_id, cache_key, cache_version, git_ref) DO UPDATE
    SET object_key       = EXCLUDED.object_key,
        size_bytes       = EXCLUDED.size_bytes,
        last_accessed_at = now()
RETURNING id, repo_id, cache_key, cache_version, git_ref, object_key,
          size_bytes, last_accessed_at, created_at;

-- name: ListWorkflowCachesForRepo :many
-- Paginated list filtered optionally by ref + key. NULL params skip
-- the filter. Sorted by last_accessed_at DESC so an operator sees the
-- live caches first.
SELECT id, repo_id, cache_key, cache_version, git_ref, object_key,
       size_bytes, last_accessed_at, created_at
FROM workflow_caches
WHERE repo_id = $1
  AND (sqlc.narg(git_ref)::text IS NULL OR git_ref = sqlc.narg(git_ref)::text)
  AND (sqlc.narg(cache_key)::text IS NULL OR cache_key = sqlc.narg(cache_key)::text)
ORDER BY last_accessed_at DESC, id DESC
LIMIT $2 OFFSET $3;

-- name: CountWorkflowCachesForRepo :one
SELECT COUNT(*) FROM workflow_caches
WHERE repo_id = $1
  AND (sqlc.narg(git_ref)::text IS NULL OR git_ref = sqlc.narg(git_ref)::text)
  AND (sqlc.narg(cache_key)::text IS NULL OR cache_key = sqlc.narg(cache_key)::text);

-- name: GetWorkflowCacheByID :one
SELECT id, repo_id, cache_key, cache_version, git_ref, object_key,
       size_bytes, last_accessed_at, created_at
FROM workflow_caches
WHERE id = $1;

-- name: DeleteWorkflowCacheByID :execrows
DELETE FROM workflow_caches WHERE id = $1 AND repo_id = $2;

-- name: DeleteWorkflowCachesByKey :many
-- Returns the deleted rows' object_keys so the handler can purge the
-- backing tarballs from object storage.
WITH deleted AS (
    DELETE FROM workflow_caches
    WHERE repo_id = $1
      AND cache_key = $2
      AND (sqlc.narg(git_ref)::text IS NULL OR git_ref = sqlc.narg(git_ref)::text)
    RETURNING object_key
)
SELECT object_key FROM deleted;

-- name: TouchWorkflowCache :exec
-- Bumps last_accessed_at on cache hit. Called by the future
-- restore-side handler.
UPDATE workflow_caches
SET last_accessed_at = now()
WHERE id = $1;
