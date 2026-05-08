-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S16 lifecycle queries. Kept in a separate file from repos.sql so the
-- mainline CRUD stays easy to read.

-- ─── repo mutations ────────────────────────────────────────────────────

-- name: RenameRepo :exec
-- Same-owner rename. The handler validates the new name shape and the
-- redirect row is INSERTed in the same tx.
UPDATE repos SET name = $2, updated_at = now() WHERE id = $1;

-- name: TransferRepoOwner :exec
-- Sets owner_user_id (or owner_org_id post-S31) on accept. The xor
-- check on the table enforces the shape. Also clears the other side
-- so a user→org transfer flips both columns atomically.
UPDATE repos
SET owner_user_id = sqlc.narg(owner_user_id)::bigint,
    owner_org_id  = sqlc.narg(owner_org_id)::bigint,
    name          = $2,
    updated_at    = now()
WHERE id = $1;

-- name: ArchiveRepo :exec
UPDATE repos SET is_archived = true, archived_at = now(), updated_at = now() WHERE id = $1;

-- name: UnarchiveRepo :exec
UPDATE repos SET is_archived = false, archived_at = NULL, updated_at = now() WHERE id = $1;

-- name: SetRepoVisibility :exec
UPDATE repos SET visibility = $2, updated_at = now() WHERE id = $1;

-- name: SoftDeleteRepoLifecycle :exec
-- Distinct name from S11's SoftDeleteRepo so future code that wants to
-- preserve the lifecycle audit-emission shape can find this one.
UPDATE repos SET deleted_at = now(), updated_at = now() WHERE id = $1;

-- name: RestoreRepo :exec
UPDATE repos SET deleted_at = NULL, updated_at = now() WHERE id = $1;

-- name: HardDeleteRepo :exec
DELETE FROM repos WHERE id = $1;


-- ─── redirects ─────────────────────────────────────────────────────────

-- name: InsertRepoRedirect :exec
-- Both old-owner FKs are nullable; pass exactly one. The CHECK
-- constraint on the table enforces the xor shape.
INSERT INTO repo_redirects (old_owner_user_id, old_owner_org_id, old_name, repo_id)
VALUES (sqlc.narg(old_owner_user_id)::bigint, sqlc.narg(old_owner_org_id)::bigint, $1, $2)
ON CONFLICT DO NOTHING;

-- name: LookupRedirectByUserOwner :one
-- Returns the current repo_id when (old_owner_user_id, old_name) hits
-- a redirect row.
SELECT repo_id FROM repo_redirects
WHERE old_owner_user_id = $1 AND old_name = $2;

-- name: DeleteRedirectsForRepo :exec
-- Used by hard-delete: drop the redirect rows pointing at this repo
-- (they would dangle once the repos row is gone; the FK ON DELETE
-- CASCADE would handle it, but explicit is auditable).
DELETE FROM repo_redirects WHERE repo_id = $1;

-- name: DeleteRedirectByUserOwnerOldName :exec
-- Used by the rename compensator: drop a single redirect row when
-- the rename has to be rolled back due to a filesystem failure. We
-- avoided raw SQL here at the audit's request (S00-S25, M).
DELETE FROM repo_redirects
WHERE repo_id = $1 AND old_owner_user_id = $2 AND old_name = $3;


-- ─── transfer requests ─────────────────────────────────────────────────

-- name: InsertTransferRequest :one
INSERT INTO repo_transfer_requests (
    repo_id, from_user_id, to_principal_kind, to_principal_id,
    created_by, expires_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
RETURNING id, repo_id, from_user_id, to_principal_kind, to_principal_id,
          created_by, created_at, expires_at, status,
          accepted_at, declined_at, canceled_at;

-- name: GetTransferRequest :one
SELECT id, repo_id, from_user_id, to_principal_kind, to_principal_id,
       created_by, created_at, expires_at, status,
       accepted_at, declined_at, canceled_at
FROM repo_transfer_requests
WHERE id = $1;

-- name: ListPendingTransfersForUser :many
-- Inbox view: pending offers a user can act on.
SELECT id, repo_id, from_user_id, to_principal_kind, to_principal_id,
       created_by, created_at, expires_at, status,
       accepted_at, declined_at, canceled_at
FROM repo_transfer_requests
WHERE to_principal_kind = 'user' AND to_principal_id = $1 AND status = 'pending'
ORDER BY created_at DESC;

-- name: ListTransfersForRepo :many
-- Sender / repo-settings view.
SELECT id, repo_id, from_user_id, to_principal_kind, to_principal_id,
       created_by, created_at, expires_at, status,
       accepted_at, declined_at, canceled_at
FROM repo_transfer_requests
WHERE repo_id = $1
ORDER BY created_at DESC;

-- name: AcceptTransferRequest :exec
UPDATE repo_transfer_requests
SET status = 'accepted', accepted_at = now()
WHERE id = $1 AND status = 'pending';

-- name: DeclineTransferRequest :exec
UPDATE repo_transfer_requests
SET status = 'declined', declined_at = now()
WHERE id = $1 AND status = 'pending';

-- name: CancelTransferRequest :exec
UPDATE repo_transfer_requests
SET status = 'canceled', canceled_at = now()
WHERE id = $1 AND status = 'pending';

-- name: ExpirePendingTransfers :execrows
-- Called by the periodic worker (transfers:expire) — flips pending
-- offers past their expires_at to the expired terminal state.
UPDATE repo_transfer_requests
SET status = 'expired'
WHERE status = 'pending' AND expires_at < now();


-- ─── soft-delete sweep query ───────────────────────────────────────────

-- name: ListRepoIDsPastSoftDeleteGrace :many
-- The repo:hard_delete enqueuer queries this to find rows ready for
-- destruction. The 7-day grace is hard-coded here; if we add a config
-- knob later, change this to a parameter.
SELECT id FROM repos
WHERE deleted_at IS NOT NULL AND deleted_at < now() - interval '7 days'
ORDER BY deleted_at ASC;

-- name: ListSoftDeletedReposForOwner :many
-- /settings/repositories/restore page lists these.
SELECT id, owner_user_id, name, deleted_at
FROM repos
WHERE owner_user_id = $1 AND deleted_at IS NOT NULL
ORDER BY deleted_at DESC;


-- ─── rename rate limit support ─────────────────────────────────────────

-- name: CountRecentRedirectsForRepo :one
-- Used to enforce the 5-per-30-days rename rate limit. The redirect
-- row is the audit trail for renames; counting them per repo gives a
-- reliable cap.
SELECT count(*)::int AS recent_count
FROM repo_redirects
WHERE repo_id = $1 AND redirected_at > now() - interval '30 days';


-- ─── fork-anchor cleanup on hard delete ────────────────────────────────

-- name: OrphanForksOf :execrows
-- Children pointing at this repo lose their fork-of pointer. Mirrors
-- GitHub's behavior when an upstream is deleted.
UPDATE repos SET fork_of_repo_id = NULL WHERE fork_of_repo_id = $1;
