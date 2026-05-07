// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// SoftDelete sets repos.deleted_at to now. The repo disappears from
// listings and the home page returns 404 for non-owners (auth-aware).
// The bare repo on disk is left alone until the hard-delete worker
// runs at the end of the grace window.
func SoftDelete(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if repo.DeletedAt.Valid {
		return ErrAlreadyDeleted
	}
	if err := rq.SoftDeleteRepoLifecycle(ctx, deps.Pool, repoID); err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoSoftDeleted, audit.TargetRepo, repoID,
			map[string]any{"name": repo.Name, "owner_user_id": int64ValueOrZero(repo.OwnerUserID.Int64, repo.OwnerUserID.Valid)})
	}
	return nil
}

// Restore clears repos.deleted_at within the grace window. Past the
// window the row may still exist (worker hasn't run yet) but we
// refuse — the operator-visible contract is "7 days, then it's gone".
func Restore(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if !repo.DeletedAt.Valid {
		return ErrNotDeleted
	}
	if deps.now().Sub(repo.DeletedAt.Time) > softDeleteGrace {
		return ErrPastGrace
	}
	if err := rq.RestoreRepo(ctx, deps.Pool, repoID); err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoRestored, audit.TargetRepo, repoID, nil)
	}
	return nil
}

// int64ValueOrZero unwraps a pgtype.Int8 stored as raw int64+bool. We
// keep this private helper duplicated across packages rather than
// pulling pgtype into the audit-meta hot path.
func int64ValueOrZero(v int64, valid bool) int64 {
	if valid {
		return v
	}
	return 0
}
