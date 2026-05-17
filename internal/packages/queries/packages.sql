-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: ListRepoPackages :many
SELECT
    p.id,
    p.repo_id,
    p.name,
    p.normalized_name,
    p.package_type,
    p.description,
    p.latest_version,
    p.package_bytes,
    p.created_by_user_id,
    p.updated_by_user_id,
    p.created_at,
    p.updated_at,
    COUNT(DISTINCT v.id)::bigint AS version_count,
    COUNT(f.id)::bigint AS file_count
FROM repo_packages p
LEFT JOIN repo_package_versions v ON v.package_id = p.id
LEFT JOIN repo_package_files f ON f.version_id = v.id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint
GROUP BY p.id
ORDER BY p.updated_at DESC, lower(p.name) ASC, p.id DESC;

-- name: GetRepoPackageForRepo :one
SELECT *
FROM repo_packages
WHERE repo_id = sqlc.arg(repo_id)::bigint
  AND id = sqlc.arg(package_id)::bigint;

-- name: UpsertRepoPackage :one
INSERT INTO repo_packages (
    repo_id,
    name,
    package_type,
    description,
    created_by_user_id,
    updated_by_user_id
)
VALUES (
    sqlc.arg(repo_id)::bigint,
    sqlc.arg(name)::text,
    sqlc.arg(package_type)::text,
    sqlc.arg(description)::text,
    sqlc.narg(user_id)::bigint,
    sqlc.narg(user_id)::bigint
)
ON CONFLICT (repo_id, normalized_name, package_type) DO UPDATE
   SET description = CASE
           WHEN EXCLUDED.description <> '' THEN EXCLUDED.description
           ELSE repo_packages.description
       END,
       updated_by_user_id = EXCLUDED.updated_by_user_id,
       updated_at = now()
RETURNING *;

-- name: EnsureRepoPackageVersion :one
INSERT INTO repo_package_versions (
    package_id,
    version,
    created_by_user_id
)
VALUES (
    sqlc.arg(package_id)::bigint,
    sqlc.arg(version)::text,
    sqlc.narg(user_id)::bigint
)
ON CONFLICT (package_id, version) DO UPDATE
   SET version = repo_package_versions.version
RETURNING *;

-- name: InsertRepoPackageFile :one
INSERT INTO repo_package_files (
    version_id,
    filename,
    object_key,
    content_type,
    size_bytes,
    etag,
    created_by_user_id
)
VALUES (
    sqlc.arg(version_id)::bigint,
    sqlc.arg(filename)::text,
    sqlc.arg(object_key)::text,
    sqlc.arg(content_type)::text,
    sqlc.arg(size_bytes)::bigint,
    sqlc.arg(etag)::text,
    sqlc.narg(user_id)::bigint
)
RETURNING *;

-- name: RefreshRepoPackageStats :one
WITH stats AS (
    SELECT
        p.id,
        COALESCE(SUM(f.size_bytes), 0)::bigint AS package_bytes,
        COALESCE((
            SELECT v.version
            FROM repo_package_versions v
            WHERE v.package_id = p.id
            ORDER BY v.created_at DESC, v.id DESC
            LIMIT 1
        ), '') AS latest_version
    FROM repo_packages p
    LEFT JOIN repo_package_versions v ON v.package_id = p.id
    LEFT JOIN repo_package_files f ON f.version_id = v.id
    WHERE p.id = sqlc.arg(package_id)::bigint
    GROUP BY p.id
)
UPDATE repo_packages p
   SET package_bytes = stats.package_bytes,
       latest_version = stats.latest_version,
       updated_at = now()
FROM stats
WHERE p.id = stats.id
RETURNING p.*;

-- name: RefreshRepoPackageVersionStats :one
WITH stats AS (
    SELECT
        v.id,
        COALESCE(SUM(f.size_bytes), 0)::bigint AS size_bytes
    FROM repo_package_versions v
    LEFT JOIN repo_package_files f ON f.version_id = v.id
    WHERE v.id = sqlc.arg(version_id)::bigint
    GROUP BY v.id
)
UPDATE repo_package_versions v
   SET size_bytes = stats.size_bytes
FROM stats
WHERE v.id = stats.id
RETURNING v.*;

-- name: ListRepoPackageVersions :many
SELECT
    v.id,
    v.package_id,
    v.version,
    v.size_bytes,
    v.created_by_user_id,
    v.created_at,
    COUNT(f.id)::bigint AS file_count
FROM repo_package_versions v
JOIN repo_packages p ON p.id = v.package_id
LEFT JOIN repo_package_files f ON f.version_id = v.id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint
  AND p.id = sqlc.arg(package_id)::bigint
GROUP BY v.id
ORDER BY v.created_at DESC, v.id DESC;

-- name: ListRepoPackageFiles :many
SELECT
    f.id,
    f.version_id,
    f.filename,
    f.object_key,
    f.content_type,
    f.size_bytes,
    f.etag,
    f.created_by_user_id,
    f.created_at,
    v.version,
    p.id AS package_id,
    p.name AS package_name,
    p.package_type,
    p.repo_id
FROM repo_package_files f
JOIN repo_package_versions v ON v.id = f.version_id
JOIN repo_packages p ON p.id = v.package_id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint
  AND p.id = sqlc.arg(package_id)::bigint
ORDER BY v.created_at DESC, v.id DESC, f.created_at DESC, f.id DESC;

-- name: GetRepoPackageFile :one
SELECT
    f.id,
    f.version_id,
    f.filename,
    f.object_key,
    f.content_type,
    f.size_bytes,
    f.etag,
    f.created_by_user_id,
    f.created_at,
    v.version,
    p.id AS package_id,
    p.name AS package_name,
    p.package_type,
    p.repo_id
FROM repo_package_files f
JOIN repo_package_versions v ON v.id = f.version_id
JOIN repo_packages p ON p.id = v.package_id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint
  AND f.id = sqlc.arg(file_id)::bigint;

-- name: ListRepoPackageObjectKeys :many
SELECT f.object_key
FROM repo_package_files f
JOIN repo_package_versions v ON v.id = f.version_id
JOIN repo_packages p ON p.id = v.package_id
WHERE p.repo_id = sqlc.arg(repo_id)::bigint
  AND p.id = sqlc.arg(package_id)::bigint
ORDER BY f.id ASC;

-- name: DeleteRepoPackage :execrows
DELETE FROM repo_packages
WHERE repo_id = sqlc.arg(repo_id)::bigint
  AND id = sqlc.arg(package_id)::bigint;
