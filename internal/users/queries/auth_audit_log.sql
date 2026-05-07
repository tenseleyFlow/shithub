-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: InsertAuditLog :exec
INSERT INTO auth_audit_log (actor_id, action, target_type, target_id, meta)
VALUES ($1, $2, $3, $4, $5);

-- name: ListAuditLogForTarget :many
SELECT id, actor_id, action, target_type, target_id, meta, created_at
FROM auth_audit_log
WHERE target_type = $1 AND target_id = $2
ORDER BY created_at DESC
LIMIT $3;
