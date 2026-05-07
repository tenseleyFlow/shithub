// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// TransferRequestParams describes a new transfer offer.
type TransferRequestParams struct {
	ActorUserID    int64
	RepoID         int64
	FromUserID     int64
	ToPrincipalKind string // "user" — "org" arrives in S31
	ToPrincipalID  int64
}

// RequestTransfer creates a pending transfer with a 7-day TTL. Returns
// the new request id. Confirmation typing ("type owner/repo to confirm")
// belongs in the handler — this function trusts its inputs.
func RequestTransfer(ctx context.Context, deps Deps, p TransferRequestParams) (int64, error) {
	if p.ToPrincipalKind != "user" {
		return 0, fmt.Errorf("transfer: principal kind %q not supported in S16 (org transfers in S31)", p.ToPrincipalKind)
	}
	if p.ToPrincipalID == p.FromUserID {
		return 0, ErrTransferToSelf
	}

	rq := reposdb.New()
	row, err := rq.InsertTransferRequest(ctx, deps.Pool, reposdb.InsertTransferRequestParams{
		RepoID:           p.RepoID,
		FromUserID:       p.FromUserID,
		ToPrincipalKind:  reposdb.TransferPrincipalKind(p.ToPrincipalKind),
		ToPrincipalID:    p.ToPrincipalID,
		CreatedBy:        p.ActorUserID,
		ExpiresAt:        pgtype.Timestamptz{Time: deps.now().Add(transferTTL), Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("insert transfer: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, p.ActorUserID,
			audit.ActionRepoTransferRequested, audit.TargetRepo, p.RepoID,
			map[string]any{
				"to_principal_kind": p.ToPrincipalKind,
				"to_principal_id":   p.ToPrincipalID,
				"transfer_id":       row.ID,
			})
	}
	return row.ID, nil
}

// AcceptTransfer flips the transfer to accepted, updates the repo's
// owner, writes a redirect row from the old owner+name, and renames
// the bare repo on disk if the recipient also has a different name
// in mind (rare; defaults to keeping the same name).
//
// The recipient is the actor in this op. We re-check status under the
// row lock so concurrent accept/cancel/decline races are settled
// deterministically by the DB.
func AcceptTransfer(ctx context.Context, deps Deps, actorUserID, transferID int64) error {
	rq := reposdb.New()
	uq := usersdb.New()

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	// Lock the transfer row to serialize racing accept/decline/cancel.
	row, err := lockTransfer(ctx, tx, transferID)
	if err != nil {
		return err
	}
	if row.Status != reposdb.TransferStatusPending {
		return ErrTransferTerminal
	}
	if !row.ExpiresAt.Valid || deps.now().After(row.ExpiresAt.Time) {
		return ErrTransferExpired
	}
	if row.ToPrincipalKind != reposdb.TransferPrincipalKindUser || row.ToPrincipalID != actorUserID {
		// The actor isn't the recipient — a 403 at the handler.
		return ErrTransferTerminal
	}

	repo, err := rq.GetRepoByID(ctx, tx, row.RepoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	oldOwnerUserID := int64(0)
	if repo.OwnerUserID.Valid {
		oldOwnerUserID = repo.OwnerUserID.Int64
	}

	// Insert redirect from the old owner/name.
	if err := rq.InsertRepoRedirect(ctx, tx, reposdb.InsertRepoRedirectParams{
		OldOwnerUserID: pgtype.Int8{Int64: oldOwnerUserID, Valid: oldOwnerUserID != 0},
		OldOwnerOrgID:  pgtype.Int8{Valid: false},
		OldName:        repo.Name,
		RepoID:         repo.ID,
	}); err != nil {
		return fmt.Errorf("insert redirect: %w", err)
	}

	// Update owner. We keep the same name; if it collides on the
	// recipient, the unique index trips and the tx rolls back.
	if err := rq.TransferRepoOwner(ctx, tx, reposdb.TransferRepoOwnerParams{
		ID:           repo.ID,
		Name:         repo.Name,
		OwnerUserID:  pgtype.Int8{Int64: actorUserID, Valid: true},
		OwnerOrgID:   pgtype.Int8{Valid: false},
	}); err != nil {
		var pgErr *pgconn.PgError
		if errAs(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameTaken
		}
		return fmt.Errorf("transfer owner: %w", err)
	}

	// Mark the transfer accepted.
	if err := rq.AcceptTransferRequest(ctx, tx, transferID); err != nil {
		return fmt.Errorf("accept request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	// FS move from the old owner's shard to the recipient's. Best-
	// effort: if the on-disk repo doesn't exist (never pushed), skip.
	oldOwner, err := uq.GetUserByID(ctx, deps.Pool, oldOwnerUserID)
	if err != nil {
		// Old owner row gone (recently deleted) — nothing to move.
		return nil
	}
	newOwner, err := uq.GetUserByID(ctx, deps.Pool, actorUserID)
	if err != nil {
		// Should never happen — recipient is the actor — but don't roll
		// back FS-wise on this rare error; the DB is the source of
		// truth and an operator can reconcile the disk path later.
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "transfer accept: new owner lookup", "error", err)
		}
		return nil
	}
	oldPath, e1 := deps.RepoFS.RepoPath(oldOwner.Username, repo.Name)
	newPath, e2 := deps.RepoFS.RepoPath(newOwner.Username, repo.Name)
	if e1 != nil || e2 != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "transfer accept: path resolve", "e1", e1, "e2", e2)
		}
		return nil
	}
	if _, err := os.Stat(oldPath); err == nil {
		if err := deps.RepoFS.Move(oldPath, newPath); err != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "transfer accept: fs move", "error", err)
			}
			return nil
		}
	}

	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoTransferAccepted, audit.TargetRepo, repo.ID,
			map[string]any{"transfer_id": transferID, "from_user_id": oldOwnerUserID})
	}
	return nil
}

// DeclineTransfer flips the row to declined. Recipient is the actor.
func DeclineTransfer(ctx context.Context, deps Deps, actorUserID, transferID int64) error {
	rq := reposdb.New()
	row, err := rq.GetTransferRequest(ctx, deps.Pool, transferID)
	if err != nil {
		return fmt.Errorf("load transfer: %w", err)
	}
	if row.Status != reposdb.TransferStatusPending {
		return ErrTransferTerminal
	}
	if row.ToPrincipalKind != reposdb.TransferPrincipalKindUser || row.ToPrincipalID != actorUserID {
		return ErrTransferTerminal
	}
	if err := rq.DeclineTransferRequest(ctx, deps.Pool, transferID); err != nil {
		return fmt.Errorf("decline: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoTransferDeclined, audit.TargetRepo, row.RepoID,
			map[string]any{"transfer_id": transferID})
	}
	return nil
}

// CancelTransfer is the sender pulling the offer. Repo-admin actors
// (owner) can cancel.
func CancelTransfer(ctx context.Context, deps Deps, actorUserID, transferID int64) error {
	rq := reposdb.New()
	row, err := rq.GetTransferRequest(ctx, deps.Pool, transferID)
	if err != nil {
		return fmt.Errorf("load transfer: %w", err)
	}
	if row.Status != reposdb.TransferStatusPending {
		return ErrTransferTerminal
	}
	if err := rq.CancelTransferRequest(ctx, deps.Pool, transferID); err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoTransferCanceled, audit.TargetRepo, row.RepoID,
			map[string]any{"transfer_id": transferID})
	}
	return nil
}

// ExpirePending sweeps every pending transfer past its expires_at.
// Returns the count of rows flipped — the worker uses this for
// observability. Audit logs are emitted in bulk via a single row that
// records the count; per-transfer audit lines would be noisy and the
// individual transfer rows themselves carry the timestamp.
func ExpirePending(ctx context.Context, deps Deps) (int64, error) {
	rq := reposdb.New()
	n, err := rq.ExpirePendingTransfers(ctx, deps.Pool)
	if err != nil {
		return 0, fmt.Errorf("expire: %w", err)
	}
	return n, nil
}

// lockTransfer reads the row inside a tx with FOR UPDATE so concurrent
// accept/decline/cancel can't double-fire. Returns the row directly —
// the caller already has the transferID in scope.
func lockTransfer(ctx context.Context, tx pgx.Tx, transferID int64) (reposdb.RepoTransferRequest, error) {
	row, err := tx.Query(ctx,
		`SELECT id, repo_id, from_user_id, to_principal_kind, to_principal_id,
                created_by, created_at, expires_at, status,
                accepted_at, declined_at, canceled_at
         FROM repo_transfer_requests
         WHERE id = $1
         FOR UPDATE`, transferID)
	if err != nil {
		return reposdb.RepoTransferRequest{}, fmt.Errorf("lock transfer: %w", err)
	}
	defer row.Close()
	if !row.Next() {
		return reposdb.RepoTransferRequest{}, errors.New("transfer not found")
	}
	var r reposdb.RepoTransferRequest
	if err := row.Scan(&r.ID, &r.RepoID, &r.FromUserID, &r.ToPrincipalKind, &r.ToPrincipalID,
		&r.CreatedBy, &r.CreatedAt, &r.ExpiresAt, &r.Status,
		&r.AcceptedAt, &r.DeclinedAt, &r.CanceledAt); err != nil {
		return reposdb.RepoTransferRequest{}, fmt.Errorf("scan transfer: %w", err)
	}
	return r, nil
}
