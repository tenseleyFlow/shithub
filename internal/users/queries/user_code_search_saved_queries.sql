-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-08a: saved search queries.

-- name: InsertCodeSearchSavedQuery :one
INSERT INTO user_code_search_saved_queries (user_id, name, query_text, kind, scope_filter)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListCodeSearchSavedQueriesForUser :many
SELECT *
FROM user_code_search_saved_queries
WHERE user_id = $1
ORDER BY updated_at DESC;

-- name: CountCodeSearchSavedQueriesForUser :one
SELECT count(*) FROM user_code_search_saved_queries WHERE user_id = $1;

-- name: GetCodeSearchSavedQuery :one
-- Scoped by user_id so an id-guess from another user is a no-op.
SELECT *
FROM user_code_search_saved_queries
WHERE id = $1 AND user_id = $2;

-- name: UpdateCodeSearchSavedQuery :exec
UPDATE user_code_search_saved_queries
SET name = $3, query_text = $4, kind = $5, scope_filter = $6, updated_at = now()
WHERE id = $1 AND user_id = $2;

-- name: DeleteCodeSearchSavedQuery :exec
DELETE FROM user_code_search_saved_queries
WHERE id = $1 AND user_id = $2;
