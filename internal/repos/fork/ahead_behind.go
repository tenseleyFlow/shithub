// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

import (
	"context"
	"errors"
	"fmt"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// AheadBehindStats describes how the fork's default branch relates
// to the source's default branch. Both numbers are commit counts:
// `Ahead` = commits in fork not in source; `Behind` = commits in
// source not in fork.
//
// `Comparable` is false when either side's default ref doesn't
// exist (e.g. an empty fork before its first push, or a source
// that's never been initialized). The UI renders "—" in that case.
type AheadBehindStats struct {
	Ahead      int
	Behind     int
	Comparable bool
}

// AheadBehind computes the stats by reading both default OIDs,
// then running `rev-list --left-right --count` inside the fork's
// bare repo. Because forks share object alternates with their
// source, the fork can resolve OIDs from the source without an
// explicit fetch — they're already reachable through the
// alternates link.
//
// This is the floor implementation. S36's perf-pass sprint adds
// caching keyed on `(fork_repo_id, fork_default_oid,
// upstream_default_oid)` so the rev-list shells out only on push
// (the deferral pointer is already in S36's spec).
func AheadBehind(ctx context.Context, deps Deps, forkRepoID int64) (AheadBehindStats, error) {
	rq := reposdb.New()
	fork, err := rq.GetRepoByID(ctx, deps.Pool, forkRepoID)
	if err != nil {
		return AheadBehindStats{}, fmt.Errorf("ahead-behind: load fork: %w", err)
	}
	if !fork.ForkOfRepoID.Valid {
		return AheadBehindStats{}, ErrSourceNotFound
	}
	source, err := rq.GetRepoByID(ctx, deps.Pool, fork.ForkOfRepoID.Int64)
	if err != nil {
		return AheadBehindStats{}, ErrSourceNotFound
	}
	if source.DeletedAt.Valid {
		return AheadBehindStats{}, ErrSourceDeleted
	}

	forkOwner, err := ownerSlug(ctx, deps, fork)
	if err != nil {
		return AheadBehindStats{}, err
	}
	sourceOwner, err := ownerSlug(ctx, deps, source)
	if err != nil {
		return AheadBehindStats{}, err
	}
	forkPath, err := deps.RepoFS.RepoPath(forkOwner, fork.Name)
	if err != nil {
		return AheadBehindStats{}, err
	}
	sourcePath, err := deps.RepoFS.RepoPath(sourceOwner, source.Name)
	if err != nil {
		return AheadBehindStats{}, err
	}

	// Resolve both default OIDs. Either side empty → not comparable.
	forkOID, err := repogit.ResolveRefOID(ctx, forkPath, fork.DefaultBranch)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return AheadBehindStats{Comparable: false}, nil
		}
		return AheadBehindStats{}, fmt.Errorf("ahead-behind: resolve fork: %w", err)
	}
	sourceOID, err := repogit.ResolveRefOID(ctx, sourcePath, source.DefaultBranch)
	if err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			return AheadBehindStats{Comparable: false}, nil
		}
		return AheadBehindStats{}, fmt.Errorf("ahead-behind: resolve source: %w", err)
	}
	if forkOID == sourceOID {
		return AheadBehindStats{Comparable: true}, nil
	}

	// Run the count inside the fork's repo. The fork's alternates
	// give it visibility into source's objects, so passing
	// `<forkOID>...<sourceOID>` resolves both ends.
	ahead, behind, err := repogit.AheadBehind(ctx, forkPath, sourceOID, forkOID)
	if err != nil {
		return AheadBehindStats{}, fmt.Errorf("ahead-behind: rev-list: %w", err)
	}
	// repogit.AheadBehind's argument order is (base, head): ahead
	// = "commits in head not in base"; we asked it for forkOID's
	// ahead-of sourceOID. So the returned ahead/behind are already
	// from the fork's perspective.
	return AheadBehindStats{Ahead: ahead, Behind: behind, Comparable: true}, nil
}
