// SPDX-License-Identifier: AGPL-3.0-or-later

package social

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

const (
	followRateMax    = 200
	followRateWindow = time.Hour
)

// FollowUser idempotently records actorUserID following targetUserID.
// On a new edge it emits a public user-scoped domain event so S42 feed
// surfaces can show "alice followed bob" style rows.
func FollowUser(ctx context.Context, deps Deps, actorUserID, targetUserID int64) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if actorUserID == targetUserID {
		return ErrCannotFollowSelf
	}
	if err := hitFollowLimit(ctx, deps, actorUserID); err != nil {
		return err
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := socialdb.New()
	inserted, err := q.FollowUser(ctx, tx, socialdb.FollowUserParams{
		FollowerUserID: actorUserID,
		FolloweeUserID: pgtype.Int8{Int64: targetUserID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("follow user: %w", err)
	}
	if inserted {
		if _, err := q.InsertDomainEvent(ctx, tx, socialdb.InsertDomainEventParams{
			ActorUserID: pgInt(actorUserID),
			Kind:        "followed_user",
			RepoID:      pgInt(0),
			SourceKind:  "user",
			SourceID:    targetUserID,
			Public:      true,
			Payload:     []byte("{}"),
		}); err != nil {
			return fmt.Errorf("follow event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true

	if inserted && deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionFollowCreated, audit.TargetUser, targetUserID, nil)
	}
	return nil
}

// UnfollowUser removes the actor->user follow edge. The operation is
// idempotent so duplicate form submits are harmless.
func UnfollowUser(ctx context.Context, deps Deps, actorUserID, targetUserID int64) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if err := hitFollowLimit(ctx, deps, actorUserID); err != nil {
		return err
	}
	rows, err := socialdb.New().UnfollowUser(ctx, deps.Pool, socialdb.UnfollowUserParams{
		FollowerUserID: actorUserID,
		FolloweeUserID: pgtype.Int8{Int64: targetUserID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("unfollow user: %w", err)
	}
	if rows > 0 && deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionFollowDeleted, audit.TargetUser, targetUserID, nil)
	}
	return nil
}

// FollowOrg idempotently records actorUserID following an organization.
// This is an S42-compatible extension over the user-only sprint text so
// org-owned repos can participate in the same social feed.
func FollowOrg(ctx context.Context, deps Deps, actorUserID, orgID int64) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if err := hitFollowLimit(ctx, deps, actorUserID); err != nil {
		return err
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := socialdb.New()
	inserted, err := q.FollowOrg(ctx, tx, socialdb.FollowOrgParams{
		FollowerUserID: actorUserID,
		FolloweeOrgID:  pgtype.Int8{Int64: orgID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("follow org: %w", err)
	}
	if inserted {
		if _, err := q.InsertDomainEvent(ctx, tx, socialdb.InsertDomainEventParams{
			ActorUserID: pgInt(actorUserID),
			Kind:        "followed_org",
			RepoID:      pgInt(0),
			SourceKind:  "org",
			SourceID:    orgID,
			Public:      true,
			Payload:     []byte("{}"),
		}); err != nil {
			return fmt.Errorf("follow event: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true

	if inserted && deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionFollowCreated, audit.TargetOrg, orgID, nil)
	}
	return nil
}

// UnfollowOrg removes an actor->org follow edge.
func UnfollowOrg(ctx context.Context, deps Deps, actorUserID, orgID int64) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if err := hitFollowLimit(ctx, deps, actorUserID); err != nil {
		return err
	}
	rows, err := socialdb.New().UnfollowOrg(ctx, deps.Pool, socialdb.UnfollowOrgParams{
		FollowerUserID: actorUserID,
		FolloweeOrgID:  pgtype.Int8{Int64: orgID, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("unfollow org: %w", err)
	}
	if rows > 0 && deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionFollowDeleted, audit.TargetOrg, orgID, nil)
	}
	return nil
}

func IsFollowingUser(ctx context.Context, deps Deps, followerUserID, targetUserID int64) (bool, error) {
	if followerUserID == 0 {
		return false, nil
	}
	return socialdb.New().IsFollowingUser(ctx, deps.Pool, socialdb.IsFollowingUserParams{
		FollowerUserID: followerUserID,
		FolloweeUserID: pgtype.Int8{Int64: targetUserID, Valid: true},
	})
}

func IsFollowingOrg(ctx context.Context, deps Deps, followerUserID, orgID int64) (bool, error) {
	if followerUserID == 0 {
		return false, nil
	}
	return socialdb.New().IsFollowingOrg(ctx, deps.Pool, socialdb.IsFollowingOrgParams{
		FollowerUserID: followerUserID,
		FolloweeOrgID:  pgtype.Int8{Int64: orgID, Valid: true},
	})
}

func hitFollowLimit(ctx context.Context, deps Deps, userID int64) error {
	if deps.Limiter == nil {
		return nil
	}
	if err := deps.Limiter.Hit(ctx, deps.Pool, throttle.Limit{
		Scope:      "follow",
		Identifier: fmt.Sprintf("user:%d", userID),
		Max:        followRateMax,
		Window:     followRateWindow,
	}); err != nil {
		return ErrFollowRateLimit
	}
	return nil
}
