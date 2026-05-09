// SPDX-License-Identifier: AGPL-3.0-or-later

package fork

import (
	"context"
	"fmt"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// ownerUsername resolves the username string for a repo owner. Only
// user-owned repos are supported today; org-owned repos surface
// when S31 lands. Returns an error rather than guessing when the
// repo isn't user-owned so the caller fails loudly during the
// transition rather than silently misroute.
func ownerUsername(ctx context.Context, deps Deps, repo reposdb.Repo) (string, error) {
	if !repo.OwnerUserID.Valid {
		return "", fmt.Errorf("fork: repo %d has no user owner (org-owned repos arrive in S31)", repo.ID)
	}
	u, err := usersdb.New().GetUserByID(ctx, deps.Pool, repo.OwnerUserID.Int64)
	if err != nil {
		return "", fmt.Errorf("fork: load owner: %w", err)
	}
	return u.Username, nil
}
