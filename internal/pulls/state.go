// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls

import (
	"context"
	"errors"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
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
	// H14: idempotent close/reopen is success here — the PR wrapper
	// passes empty `reason` so the API caller never sent a meaningful
	// state_reason. The sentinel just signals "no transition happened";
	// callers of pulls.SetState don't surface that further.
	if err := issues.SetState(ctx, issues.Deps{Pool: deps.Pool, Logger: deps.Logger}, actorUserID, prID, newState, ""); err != nil && !errors.Is(err, issues.ErrAlreadyInState) {
		return err
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionPullStateChanged, audit.TargetPull, prID,
			map[string]any{"state": newState})
	}
	return nil
}
