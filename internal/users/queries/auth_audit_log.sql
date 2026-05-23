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

-- name: ListOrgAuditLog :many
-- Organization owner view. Includes direct org events and repository
-- events for repositories owned by the organization, with the same
-- filtering surface as the site-admin audit viewer.
SELECT al.id, al.actor_id, al.action, al.target_type, al.target_id, al.meta, al.created_at
FROM auth_audit_log al
WHERE (
    (al.target_type = 'org' AND al.target_id = sqlc.arg(org_id)::bigint)
    OR (
        al.target_type = 'repo'
        AND al.target_id IN (
            SELECT r.id
            FROM repos r
            WHERE r.owner_org_id = sqlc.arg(org_id)::bigint
        )
    )
)
AND (sqlc.narg(actor_id)::bigint IS NULL OR al.actor_id = sqlc.narg(actor_id)::bigint)
AND (sqlc.narg(action_prefix)::text IS NULL OR al.action ILIKE sqlc.narg(action_prefix)::text || '%')
AND (sqlc.narg(target_type)::text IS NULL OR al.target_type = sqlc.narg(target_type)::text)
AND (sqlc.narg(target_id)::bigint IS NULL OR al.target_id = sqlc.narg(target_id)::bigint)
AND (sqlc.narg(since)::timestamptz IS NULL OR al.created_at >= sqlc.narg(since)::timestamptz)
AND (sqlc.narg(until)::timestamptz IS NULL OR al.created_at < sqlc.narg(until)::timestamptz)
ORDER BY al.created_at DESC, al.id DESC
LIMIT sqlc.arg(limit_count)::int OFFSET sqlc.arg(offset_count)::int;
