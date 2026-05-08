// SPDX-License-Identifier: AGPL-3.0-or-later

package social

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

// WatchLevel mirrors the DB enum so handlers can pass typed values
// without importing socialdb.
type WatchLevel string

const (
	WatchAll           WatchLevel = "all"
	WatchParticipating WatchLevel = "participating"
	WatchIgnore        WatchLevel = "ignore"
)

func (w WatchLevel) valid() bool {
	switch w {
	case WatchAll, WatchParticipating, WatchIgnore:
		return true
	}
	return false
}

// SetWatch upserts an explicit watch level. The handler authorizes
// via policy.Can(ActionWatchSet, repo) before calling.
func SetWatch(ctx context.Context, deps Deps, actorUserID, repoID int64, level WatchLevel) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if !level.valid() {
		return ErrInvalidWatchLevel
	}
	if err := socialdb.New().UpsertWatch(ctx, deps.Pool, socialdb.UpsertWatchParams{
		UserID: actorUserID,
		RepoID: repoID,
		Level:  socialdb.WatchLevel(level),
	}); err != nil {
		return fmt.Errorf("upsert watch: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionWatchSet, audit.TargetRepo, repoID,
			map[string]any{"level": string(level)})
	}
	return nil
}

// UnsetWatch removes the explicit row, returning the user to the
// implicit `participating` default.
func UnsetWatch(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if err := socialdb.New().DeleteWatch(ctx, deps.Pool, socialdb.DeleteWatchParams{
		UserID: actorUserID, RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("delete watch: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionWatchUnset, audit.TargetRepo, repoID, nil)
	}
	return nil
}

// CurrentLevel returns the actor's current watch level for the repo,
// resolving the implicit `participating` default when no row exists.
func CurrentLevel(ctx context.Context, deps Deps, userID, repoID int64) (WatchLevel, error) {
	if userID == 0 {
		return WatchParticipating, nil
	}
	row, err := socialdb.New().GetWatch(ctx, deps.Pool, socialdb.GetWatchParams{
		UserID: userID, RepoID: repoID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return WatchParticipating, nil
		}
		return "", err
	}
	return WatchLevel(row.Level), nil
}
