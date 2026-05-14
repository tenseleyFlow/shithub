// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

import (
	"context"
	"fmt"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// ownerSlug resolves the on-disk path segment for a repo owner.
// Handles both user-owned and org-owned repos: RepoFS keys paths
// as <owner-slug>/<repo-name> and doesn't distinguish kinds, so
// the resolver just needs to return whichever identifier matches.
//
// Mirrored in internal/worker/jobs/repo_fork_clone.go to keep the
// worker free of the orchestrator package's import graph.
func ownerSlug(ctx context.Context, deps Deps, repo reposdb.Repo) (string, error) {
	switch {
	case repo.OwnerUserID.Valid:
		u, err := usersdb.New().GetUserByID(ctx, deps.Pool, repo.OwnerUserID.Int64)
		if err != nil {
			return "", fmt.Errorf("fork: load user owner: %w", err)
		}
		return u.Username, nil
	case repo.OwnerOrgID.Valid:
		o, err := orgsdb.New().GetOrgByID(ctx, deps.Pool, repo.OwnerOrgID.Int64)
		if err != nil {
			return "", fmt.Errorf("fork: load org owner: %w", err)
		}
		return o.Slug, nil
	default:
		return "", fmt.Errorf("fork: repo %d has neither user nor org owner", repo.ID)
	}
}
