-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: GetMeta :one
SELECT key, value, updated_at
FROM meta
WHERE key = $1;

-- name: SetMeta :exec
INSERT INTO meta (key, value)
VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE
    SET value = EXCLUDED.value;

-- name: ListMeta :many
SELECT key, value, updated_at
FROM meta
ORDER BY key;

-- name: DeleteMeta :exec
DELETE FROM meta WHERE key = $1;
