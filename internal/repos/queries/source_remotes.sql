-- SPDX-License-Identifier: AGPL-3.0-or-later

-- name: GetRepoSourceRemote :one
SELECT repo_id, remote_url, last_fetched_at, last_error, created_at, updated_at
FROM repo_source_remotes
WHERE repo_id = $1;

-- name: UpsertRepoSourceRemote :one
INSERT INTO repo_source_remotes (repo_id, remote_url, last_error)
VALUES ($1, $2, NULL)
ON CONFLICT (repo_id) DO UPDATE
   SET remote_url = EXCLUDED.remote_url,
       last_error = NULL,
       updated_at = now()
RETURNING repo_id, remote_url, last_fetched_at, last_error, created_at, updated_at;

-- name: DeleteRepoSourceRemote :exec
DELETE FROM repo_source_remotes
WHERE repo_id = $1;

-- name: MarkRepoSourceRemoteFetched :exec
UPDATE repo_source_remotes
   SET last_fetched_at = now(),
       last_error = NULL,
       updated_at = now()
 WHERE repo_id = $1;

-- name: MarkRepoSourceRemoteFetchError :exec
UPDATE repo_source_remotes
   SET last_error = $2,
       updated_at = now()
 WHERE repo_id = $1;
