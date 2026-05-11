// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// HardDelete is the worker-driven cascade that runs after the soft-
// delete grace expires. The order is explicit and auditable:
//
//  1. Anonymize forks-of-this-repo (clear fork_of_repo_id on children).
//  2. Drop the redirect rows that point at this repo (other repos
//     keeping us as their old name; keep redirects pointed away from
//     us so old URLs of *this* repo's previous identity are still
//     resolvable from the now-deleted row, until that repo is also
//     deleted by ON DELETE CASCADE on the FK).
//  3. DELETE FROM repos — FK cascades handle push_events,
//     webhook_events_pending, repo_collaborators, transfer requests,
//     and any redirect rows pointing at this id.
//  4. Remove the bare repo tombstone on disk. For legacy soft-deleted
//     rows that still occupy the canonical path, remove that path only
//     when no new active repo has reused the owner/name.
//  5. Audit-log the hard-delete with a snapshot of the removed row in
//     the meta payload so the audit row is self-contained even after
//     the repo_id is gone.
//
// The op refuses to run on a repo that isn't past its soft-delete
// grace window — defense-in-depth even though the worker query
// already gates this. ActorUserID is typically the worker's instance
// id encoded in some way; pass 0 to record "system" in audit.
func HardDelete(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
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
	if deps.now().Sub(repo.DeletedAt.Time) < softDeleteGrace {
		return fmt.Errorf("hard-delete: %w", ErrPastGrace)
		// Sentinel mismatch on purpose — the only "before grace"
		// failure is the worker mis-firing. Reuse ErrPastGrace's
		// inverse semantics to keep the error surface tight.
	}

	// Snapshot for the audit row before we mutate.
	snapshot := map[string]any{
		"id":         repo.ID,
		"name":       repo.Name,
		"visibility": string(repo.Visibility),
	}
	if repo.OwnerUserID.Valid {
		snapshot["owner_user_id"] = repo.OwnerUserID.Int64
	}
	if repo.OwnerOrgID.Valid {
		snapshot["owner_org_id"] = repo.OwnerOrgID.Int64
	}
	if ownerSlug, err := ownerSlugForRepo(ctx, deps, repo); err == nil {
		snapshot["owner"] = ownerSlug
	}
	paths, pathErr := diskPathsForRepo(ctx, deps, repo)
	activeNameExists, err := activeRepoNameExists(ctx, rq, tx, repo)
	if err != nil {
		return fmt.Errorf("hard-delete name check: %w", err)
	}

	// 1. Orphan child forks.
	if _, err := rq.OrphanForksOf(ctx, tx, pgtype.Int8{Int64: repoID, Valid: true}); err != nil {
		return fmt.Errorf("orphan forks: %w", err)
	}

	// 2. (Skipped — see comment in the doc block. The repo's outgoing
	//    redirect rows go in step 3 via FK cascade.)

	// 3. Drop the repo row. FK ON DELETE CASCADE on push_events,
	//    repo_collaborators, repo_redirects, repo_transfer_requests,
	//    and webhook_events_pending makes this a single SQL.
	if err := rq.HardDeleteRepo(ctx, tx, repoID); err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}

	// 4. Remove the bare repo while the owner/name advisory lock is
	// still held. That matters for legacy soft-deleted rows whose data
	// is still at the canonical path: a concurrent recreate cannot race
	// this cleanup and have its new bare repo removed.
	if pathErr == nil {
		if err := removeDeletedRepoPath(deps, paths, activeNameExists); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "hard delete: fs delete failed",
				"repo_id", repoID, "path", paths.deleted, "canonical_path", paths.canonical, "error", err)
		}
	} else if deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "hard delete: compute fs path failed", "repo_id", repoID, "error", pathErr)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true

	// 5. Audit. Use a fresh pool conn since the repo_id no longer
	//    exists; the meta blob carries the snapshot.
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoHardDeleted, audit.TargetRepo, repoID, snapshot)
	}
	return nil
}

func removeDeletedRepoPath(deps Deps, paths repoDiskPaths, activeNameExists bool) error {
	deletedExists, err := deps.RepoFS.Exists(paths.deleted)
	if err != nil {
		return err
	}
	if deletedExists {
		return deps.RepoFS.Delete(paths.deleted)
	}
	if activeNameExists {
		return nil
	}
	return deps.RepoFS.Delete(paths.canonical)
}
