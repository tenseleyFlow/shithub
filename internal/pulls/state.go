// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls

import (
	"context"
	"errors"

	"github.com/tenseleyFlow/shithub/internal/issues"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// SetState delegates to issues.SetState with a couple of PR-specific
// guardrails:
//
//   - reopening a merged PR is a no-op (a merged PR can't reopen).
//   - reopening when base or head no longer resolves returns a typed
//     error so the handler can render "branches moved, cannot reopen".
func SetState(ctx context.Context, deps Deps, gitDir string, actorUserID, prID int64, newState string) error {
	q := pullsdb.New()
	pr, err := q.GetPullRequestByIssueID(ctx, deps.Pool, prID)
	if err != nil {
		return ErrPRNotFound
	}
	if pr.MergedAt.Valid {
		return ErrAlreadyMerged
	}
	if newState == "open" {
		// Validate refs still resolve; otherwise reopen would leave a PR
		// pointing at a missing branch which the diff renderer can't
		// handle cleanly.
		if _, err := repogit.ResolveRefOID(ctx, gitDir, pr.BaseRef); err != nil {
			if errors.Is(err, repogit.ErrRefNotFound) {
				return ErrBaseNotFound
			}
			return err
		}
		if _, err := repogit.ResolveRefOID(ctx, gitDir, pr.HeadRef); err != nil {
			if errors.Is(err, repogit.ErrRefNotFound) {
				return ErrHeadNotFound
			}
			return err
		}
	}
	return issues.SetState(ctx, issues.Deps{Pool: deps.Pool, Logger: deps.Logger}, actorUserID, prID, newState, "")
}
