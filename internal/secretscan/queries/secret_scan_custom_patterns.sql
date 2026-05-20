-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- SP26a: organization custom secret patterns.

-- name: CreateSecretScanCustomPattern :one
INSERT INTO secret_scan_custom_patterns (
    org_id, name, description, pattern, min_match_len, enabled, created_by, updated_by
)
VALUES ($1, $2, $3, $4, $5, true, $6, $6)
RETURNING *;

-- name: UpdateSecretScanCustomPattern :one
UPDATE secret_scan_custom_patterns
SET name = $3,
    description = $4,
    pattern = $5,
    min_match_len = $6,
    updated_by = $7
WHERE id = $1
  AND org_id = $2
RETURNING *;

-- name: SetSecretScanCustomPatternEnabled :one
UPDATE secret_scan_custom_patterns
SET enabled = $3,
    updated_by = $4
WHERE id = $1
  AND org_id = $2
RETURNING *;

-- name: DeleteSecretScanCustomPattern :exec
DELETE FROM secret_scan_custom_patterns
WHERE id = $1
  AND org_id = $2;

-- name: GetSecretScanCustomPattern :one
SELECT *
FROM secret_scan_custom_patterns
WHERE id = $1
  AND org_id = $2;

-- name: ListSecretScanCustomPatternsForOrg :many
SELECT *
FROM secret_scan_custom_patterns
WHERE org_id = $1
ORDER BY enabled DESC, lower(name), id;

-- name: ListEnabledSecretScanCustomPatternsForOrg :many
SELECT *
FROM secret_scan_custom_patterns
WHERE org_id = $1
  AND enabled
ORDER BY lower(name), id;
