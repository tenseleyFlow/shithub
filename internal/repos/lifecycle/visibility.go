// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"errors"
	"fmt"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// ErrInvalidVisibility is returned when a caller passes a value that
// isn't "public" or "private". Handlers should map to a 400.
var ErrInvalidVisibility = errors.New("lifecycle: visibility must be public or private")

// SetVisibility flips a repo between public and private. Forks remain
// independent (separate repos at the data layer), and existing clones
// already pulled remain — that's git's nature, not a bug. The UI is
// responsible for surfacing the "future clones will be private"
// caveat to the user.
func SetVisibility(ctx context.Context, deps Deps, actorUserID, repoID int64, newVisibility string) error {
	switch newVisibility {
	case "public", "private":
	default:
		return ErrInvalidVisibility
	}

	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if string(repo.Visibility) == newVisibility {
		return nil // idempotent no-op
	}
	if repo.OwnerOrgID.Valid && newVisibility == "private" {
		check, err := entitlements.CheckRepoPrivateVisibility(ctx, entitlements.Deps{Pool: deps.Pool}, repo.OwnerOrgID.Int64, repo.ID)
		if err != nil {
			return err
		}
		if err := check.Err(); err != nil {
			return err
		}
	}

	if err := rq.SetRepoVisibility(ctx, deps.Pool, reposdb.SetRepoVisibilityParams{
		ID:         repoID,
		Visibility: reposdb.RepoVisibility(newVisibility),
	}); err != nil {
		return fmt.Errorf("set visibility: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoVisibilityChanged, audit.TargetRepo, repoID,
			map[string]any{"from": string(repo.Visibility), "to": newVisibility})
	}
	return nil
}
