// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// SyncResult describes what Sync did. Reasons cover the four
// outcomes the spec enumerates: fast-forwarded, already up to date,
// diverged (refused), or default-branch-missing (typically a freshly
// initialized fork or upstream that's never been pushed to).
type SyncResult struct {
	OldOID string
	NewOID string
}

// Sync fast-forwards the fork's default branch to the upstream's.
// Refuses on diverged history (as the spec mandates — anything else
// belongs in the user's client). The CAS via UpdateRefCAS catches
// concurrent pushes to the fork: if a push lands between our read
// and our update, we return ErrSyncRefRaced and the caller can ask
// the user to retry.
//
// The handler MUST authorize via policy.Can(ActionRepoWrite) on the
// fork before calling — Sync mutates the fork's refs and is a write
// action. Read access on the source is implied by ownership of the
// fork (you forked it; you saw it then).
func Sync(ctx context.Context, deps Deps, actorUserID, forkRepoID int64) (SyncResult, error) {
	rq := reposdb.New()
	fork, err := rq.GetRepoByID(ctx, deps.Pool, forkRepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SyncResult{}, ErrSourceNotFound
		}
		return SyncResult{}, fmt.Errorf("sync: load fork: %w", err)
	}
	if fork.InitStatus != reposdb.RepoInitStatusInitialized {
		return SyncResult{}, ErrForkNotInitialized
	}
	if !fork.ForkOfRepoID.Valid {
		return SyncResult{}, ErrSourceNotFound
	}
	source, err := rq.GetRepoByID(ctx, deps.Pool, fork.ForkOfRepoID.Int64)
	if err != nil {
		// Source was hard-deleted (orphaned fork). Sync isn't
		// applicable; surface as not-found.
		return SyncResult{}, ErrSourceNotFound
	}
	if source.DeletedAt.Valid {
		return SyncResult{}, ErrSourceDeleted
	}

	forkOwner, err := ownerUsername(ctx, deps, fork)
	if err != nil {
		return SyncResult{}, err
	}
	sourceOwner, err := ownerUsername(ctx, deps, source)
	if err != nil {
		return SyncResult{}, err
	}
	forkPath, err := deps.RepoFS.RepoPath(forkOwner, fork.Name)
	if err != nil {
		return SyncResult{}, err
	}
	sourcePath, err := deps.RepoFS.RepoPath(sourceOwner, source.Name)
	if err != nil {
		return SyncResult{}, err
	}

	// Fork and source must agree on the default branch name. If they
	// don't, the fast-forward gate doesn't have an obvious answer and
	// we refuse — the user's client can sync arbitrary refs if they
	// really want to.
	branch := fork.DefaultBranch
	if branch == "" || source.DefaultBranch == "" {
		return SyncResult{}, ErrSyncDefaultsMissing
	}

	upstreamOID, err := repogit.ResolveRefOID(ctx, sourcePath, branch)
	if err != nil {
		return SyncResult{}, fmt.Errorf("sync: resolve upstream %s: %w", branch, err)
	}
	forkOID, err := repogit.ResolveRefOID(ctx, forkPath, branch)
	if err != nil {
		// Fork branch doesn't exist (an empty fork) — fall through
		// to the create-ref path below by passing the zero OID. git
		// update-ref accepts the literal 40-zero string as "must not
		// exist yet" semantics.
		forkOID = "0000000000000000000000000000000000000000"
	}
	if forkOID == upstreamOID {
		return SyncResult{}, ErrSyncUpToDate
	}
	if forkOID != "0000000000000000000000000000000000000000" {
		ancestor, err := repogit.IsAncestor(ctx, forkPath, forkOID, upstreamOID)
		if err != nil {
			return SyncResult{}, fmt.Errorf("sync: ancestor check: %w", err)
		}
		if !ancestor {
			// fork has commits upstream doesn't — diverged.
			return SyncResult{}, ErrSyncDiverged
		}
	}

	// CAS update. The fork's bare repo holds the alternates pointing
	// at source's objects, so the upstream OID is reachable from the
	// fork's perspective without an explicit fetch.
	ref := "refs/heads/" + branch
	if err := repogit.UpdateRefCAS(ctx, forkPath, ref, upstreamOID, forkOID); err != nil {
		if errors.Is(err, repogit.ErrRefRaced) {
			return SyncResult{}, ErrSyncRefRaced
		}
		return SyncResult{}, fmt.Errorf("sync: update-ref: %w", err)
	}

	// Update the cached default_branch_oid on the fork so the home
	// view reflects the new tip without waiting for a push:process
	// tick (update-ref bypasses the post-receive hook here, same
	// reason as the merge handler's fix in the audit-remediation
	// sprint).
	_ = rq.UpdateRepoDefaultBranchOID(ctx, deps.Pool, reposdb.UpdateRepoDefaultBranchOIDParams{
		ID:               fork.ID,
		DefaultBranchOid: pgtype.Text{String: upstreamOID, Valid: true},
	})

	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoForkSynced, audit.TargetRepo, fork.ID,
			map[string]any{"old_oid": forkOID, "new_oid": upstreamOID, "branch": branch})
	}
	return SyncResult{OldOID: forkOID, NewOID: upstreamOID}, nil
}
