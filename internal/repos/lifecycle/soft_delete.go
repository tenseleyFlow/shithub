// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// SoftDelete sets repos.deleted_at to now. The repo disappears from
// listings and the home page returns 404 for non-owners (auth-aware).
// The bare repo is moved from the canonical owner/name path into an
// internal tombstone path so the owner can recreate the same repo name
// during the grace window without colliding with the deleted repo.
func SoftDelete(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	rq := reposdb.New()

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

	repo, err := rq.GetRepoByID(ctx, tx, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if err := lockRepoName(ctx, rq, tx, repo); err != nil {
		return err
	}
	if repo.DeletedAt.Valid {
		return ErrAlreadyDeleted
	}
	moved, err := moveCanonicalToDeleted(ctx, deps, repo)
	if err != nil {
		return err
	}
	if err := rq.SoftDeleteRepoLifecycle(ctx, tx, repoID); err != nil {
		if moved {
			moveDeletedBackToCanonical(ctx, deps, repo)
		}
		return fmt.Errorf("soft delete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if moved {
			moveDeletedBackToCanonical(ctx, deps, repo)
		}
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
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

	repo, err := rq.GetRepoByID(ctx, tx, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if err := lockRepoName(ctx, rq, tx, repo); err != nil {
		return err
	}
	if !repo.DeletedAt.Valid {
		return ErrNotDeleted
	}
	if deps.now().Sub(repo.DeletedAt.Time) > softDeleteGrace {
		return ErrPastGrace
	}
	taken, err := activeRepoNameExists(ctx, rq, tx, repo)
	if err != nil {
		return fmt.Errorf("restore name check: %w", err)
	}
	if taken {
		return ErrNameTaken
	}
	moved, err := moveDeletedToCanonical(ctx, deps, repo)
	if err != nil {
		if errors.Is(err, storage.ErrAlreadyExists) {
			return ErrNameTaken
		}
		return err
	}
	if err := rq.RestoreRepo(ctx, tx, repoID); err != nil {
		if moved {
			moveCanonicalBackToDeleted(ctx, deps, repo)
		}
		if isUniqueViolation(err) {
			return ErrNameTaken
		}
		return fmt.Errorf("restore: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		if moved {
			moveCanonicalBackToDeleted(ctx, deps, repo)
		}
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoRestored, audit.TargetRepo, repoID, nil)
	}
	return nil
}

func moveCanonicalToDeleted(ctx context.Context, deps Deps, repo reposdb.Repo) (bool, error) {
	paths, err := diskPathsForRepo(ctx, deps, repo)
	if err != nil {
		return false, err
	}
	if err := deps.RepoFS.Move(paths.canonical, paths.deleted); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			exists, existsErr := deps.RepoFS.Exists(paths.deleted)
			if existsErr != nil {
				return false, existsErr
			}
			if exists {
				return false, nil
			}
		}
		return false, fmt.Errorf("move repo to deleted path: %w", err)
	}
	return true, nil
}

func moveDeletedToCanonical(ctx context.Context, deps Deps, repo reposdb.Repo) (bool, error) {
	paths, err := diskPathsForRepo(ctx, deps, repo)
	if err != nil {
		return false, err
	}
	if err := deps.RepoFS.Move(paths.deleted, paths.canonical); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			exists, existsErr := deps.RepoFS.Exists(paths.canonical)
			if existsErr != nil {
				return false, existsErr
			}
			if exists {
				return false, nil
			}
		}
		return false, fmt.Errorf("move repo to canonical path: %w", err)
	}
	return true, nil
}

func moveDeletedBackToCanonical(ctx context.Context, deps Deps, repo reposdb.Repo) {
	paths, err := diskPathsForRepo(ctx, deps, repo)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "soft delete: compute rollback path failed", "repo_id", repo.ID, "error", err)
		}
		return
	}
	if err := deps.RepoFS.Move(paths.deleted, paths.canonical); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "soft delete: rollback fs move failed",
			"repo_id", repo.ID, "from", paths.deleted, "to", paths.canonical, "error", err)
	}
}

func moveCanonicalBackToDeleted(ctx context.Context, deps Deps, repo reposdb.Repo) {
	paths, err := diskPathsForRepo(ctx, deps, repo)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "restore: compute rollback path failed", "repo_id", repo.ID, "error", err)
		}
		return
	}
	if err := deps.RepoFS.Move(paths.canonical, paths.deleted); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "restore: rollback fs move failed",
			"repo_id", repo.ID, "from", paths.canonical, "to", paths.deleted, "error", err)
	}
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
