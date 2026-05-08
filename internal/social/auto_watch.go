// SPDX-License-Identifier: AGPL-3.0-or-later

package social

import (
	"context"
	"fmt"

	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

// AutoWatchOnCollab inserts a `level='all'` row when no preference
// exists. Triggered by S15's collaborator-add path. Non-destructive:
// if the user has already chosen a level (including `ignore`), their
// choice is preserved.
//
// Matches GitHub: collaborators get full notifications by default;
// they can opt down to `participating` or `ignore` later.
func AutoWatchOnCollab(ctx context.Context, deps Deps, userID, repoID int64) error {
	return autoWatch(ctx, deps, userID, repoID, WatchAll)
}

// AutoWatchOnInvolvement inserts a `level='participating'` row when
// no preference exists. Triggered by issues.AddComment, mention
// resolution, assignment, review-requested. Non-destructive.
//
// Matches GitHub: any first-touch involvement (commenting, getting
// mentioned, getting assigned) auto-subscribes the user to that
// thread's notifications, but only at the participating level so a
// drive-by mention doesn't flood them with future events.
func AutoWatchOnInvolvement(ctx context.Context, deps Deps, userID, repoID int64) error {
	return autoWatch(ctx, deps, userID, repoID, WatchParticipating)
}

// autoWatch is the shared implementation. The InsertIfAbsent query's
// ON CONFLICT DO NOTHING is what makes this safe to call repeatedly
// from any orchestrator without coordinating "first call" semantics.
func autoWatch(ctx context.Context, deps Deps, userID, repoID int64, level WatchLevel) error {
	if userID == 0 {
		// Anonymous can't auto-watch. Not an error — the caller may
		// not know whether the actor is logged in.
		return nil
	}
	if err := socialdb.New().InsertWatchIfAbsent(ctx, deps.Pool, socialdb.InsertWatchIfAbsentParams{
		UserID: userID,
		RepoID: repoID,
		Level:  socialdb.WatchLevel(level),
	}); err != nil {
		return fmt.Errorf("auto-watch %s: %w", level, err)
	}
	return nil
}
