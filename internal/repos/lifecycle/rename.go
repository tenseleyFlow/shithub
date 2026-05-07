// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/repos"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// RenameParams is what a same-owner rename takes.
type RenameParams struct {
	ActorUserID int64 // who is initiating; recorded in audit
	RepoID      int64
	OwnerUserID int64  // current owner — also gives us the FS path
	OwnerName   string // current owner username (for FS path)
	OldName     string // current repo name
	NewName     string // requested name
}

// Rename performs a same-owner rename: validate → tx (insert redirect,
// update repos.name) → FS Move → audit. On FS failure the DB tx is
// reverted via a compensating UPDATE so the persistent state is
// consistent. The caller is responsible for policy.Can; we don't
// re-check here.
func Rename(ctx context.Context, deps Deps, p RenameParams) error {
	newName := strings.ToLower(strings.TrimSpace(p.NewName))
	if newName == strings.ToLower(p.OldName) {
		return ErrSameName
	}
	if err := repos.ValidateName(newName); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidName, err)
	}

	// Rate limit: count recent renames (= redirect rows for this repo).
	rq := reposdb.New()
	count, err := rq.CountRecentRedirectsForRepo(ctx, deps.Pool, p.RepoID)
	if err != nil {
		return fmt.Errorf("count redirects: %w", err)
	}
	if int(count) >= renameRateLimitMax {
		return ErrRenameRateLimited
	}

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

	// 1. Insert redirect row before mutating the repo so a unique-name
	//    collision rolls back both with a single ROLLBACK.
	if err := rq.InsertRepoRedirect(ctx, tx, reposdb.InsertRepoRedirectParams{
		OldOwnerUserID: pgtype.Int8{Int64: p.OwnerUserID, Valid: true},
		OldOwnerOrgID:  pgtype.Int8{Valid: false},
		OldName:        p.OldName,
		RepoID:         p.RepoID,
	}); err != nil {
		return fmt.Errorf("insert redirect: %w", err)
	}

	// 2. Update the name. Unique-violation here means the new name is
	//    taken on this owner.
	if err := rq.RenameRepo(ctx, tx, reposdb.RenameRepoParams{ID: p.RepoID, Name: newName}); err != nil {
		var pgErr *pgconn.PgError
		if errAs(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameTaken
		}
		return fmt.Errorf("rename repo: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	// 3. FS rename. On failure, run the compensating SQL so DB matches
	//    disk again. The compensating tx is best-effort logged; if it
	//    also fails the operator must reconcile by hand (rare path).
	oldPath, e1 := deps.RepoFS.RepoPath(p.OwnerName, p.OldName)
	newPath, e2 := deps.RepoFS.RepoPath(p.OwnerName, newName)
	if e1 != nil || e2 != nil {
		compensateRename(ctx, deps, p.RepoID, p.OldName, p.OwnerUserID, newName)
		return fmt.Errorf("repo path resolution: e1=%v e2=%v", e1, e2)
	}
	if _, err := os.Stat(oldPath); err == nil {
		if err := deps.RepoFS.Move(oldPath, newPath); err != nil {
			compensateRename(ctx, deps, p.RepoID, p.OldName, p.OwnerUserID, newName)
			return fmt.Errorf("fs move: %w", err)
		}
	}
	// (oldPath missing is acceptable — the on-disk repo may not exist
	// yet for a freshly-created repo whose initial commit hasn't been
	// pushed by the user. The bare-repo init is creation-time; rename
	// just needs to handle the present-or-absent case symmetrically.)

	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, p.ActorUserID,
			audit.ActionRepoRenamed, audit.TargetRepo, p.RepoID,
			map[string]any{"old_name": p.OldName, "new_name": newName})
	}
	return nil
}

// compensateRename undoes the redirect+name change after an FS failure.
// Logged at warn so the operator notices but the request still returns
// the original error (the caller sees the FS error, which is the user-
// visible truth).
func compensateRename(ctx context.Context, deps Deps, repoID int64, oldName string, ownerUserID int64, newName string) {
	rq := reposdb.New()
	if err := rq.RenameRepo(ctx, deps.Pool, reposdb.RenameRepoParams{ID: repoID, Name: oldName}); err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "rename: compensating UPDATE failed", "repo_id", repoID, "error", err)
		}
	}
	// Drop the redirect row we just wrote — it now points at a name
	// that's about to come back to its old self.
	_, err := deps.Pool.Exec(ctx,
		`DELETE FROM repo_redirects WHERE repo_id = $1 AND old_owner_user_id = $2 AND old_name = $3`,
		repoID, ownerUserID, oldName)
	if err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "rename: compensating redirect-delete failed", "repo_id", repoID, "error", err)
	}
	_ = newName // kept in signature for symmetry / future logging
}
