-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: GetOrgSecuritySettings :one
SELECT org_id, require_two_factor, created_at, updated_at
FROM org_security_settings
WHERE org_id = $1;

-- name: UpsertOrgSecuritySettings :one
INSERT INTO org_security_settings (org_id, require_two_factor)
VALUES ($1, $2)
ON CONFLICT (org_id) DO UPDATE
SET require_two_factor = EXCLUDED.require_two_factor,
    updated_at = now()
RETURNING org_id, require_two_factor, created_at, updated_at;
