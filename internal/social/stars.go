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

// starRateMax is the spec's day-1 rate cap: 100 star/unstar actions
// per hour per user. Defends against trending-manipulation abuse
// (S35 owns the abuse-heuristics layer; this is the floor).
const (
	starRateMax    = 100
	starRateWindow = time.Hour
)

// Star idempotently records a star. Re-starring an already-starred
// repo is a no-op (the underlying INSERT … ON CONFLICT DO NOTHING +
// AFTER INSERT trigger combination keeps star_count consistent).
//
// The handler MUST authorize via policy.Can(ActionStarCreate, repo)
// before calling — this orchestrator trusts that the actor has read
// access to the repo and is logged in.
//
// Emits a `star` event into domain_events with `public = repoIsPublic`.
func Star(ctx context.Context, deps Deps, actorUserID, repoID int64, repoIsPublic bool) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if err := hitStarLimit(ctx, deps, actorUserID); err != nil {
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
	if err := q.InsertStar(ctx, tx, socialdb.InsertStarParams{
		UserID: actorUserID, RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("insert star: %w", err)
	}
	if _, err := q.InsertDomainEvent(ctx, tx, socialdb.InsertDomainEventParams{
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: true},
		Kind:        "star",
		RepoID:      pgtype.Int8{Int64: repoID, Valid: true},
		SourceKind:  "repo",
		SourceID:    repoID,
		Public:      repoIsPublic,
		Payload:     []byte("{}"),
	}); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionStarCreated, audit.TargetRepo, repoID, nil)
	}
	return nil
}

// Unstar removes the star idempotently. Unstarring a non-starred
// repo is a no-op. Emits an `unstar` event for symmetry.
func Unstar(ctx context.Context, deps Deps, actorUserID, repoID int64, repoIsPublic bool) error {
	if actorUserID == 0 {
		return ErrNotLoggedIn
	}
	if err := hitStarLimit(ctx, deps, actorUserID); err != nil {
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
	if err := q.DeleteStar(ctx, tx, socialdb.DeleteStarParams{
		UserID: actorUserID, RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("delete star: %w", err)
	}
	if _, err := q.InsertDomainEvent(ctx, tx, socialdb.InsertDomainEventParams{
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: true},
		Kind:        "unstar",
		RepoID:      pgtype.Int8{Int64: repoID, Valid: true},
		SourceKind:  "repo",
		SourceID:    repoID,
		Public:      repoIsPublic,
		Payload:     []byte("{}"),
	}); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionStarDeleted, audit.TargetRepo, repoID, nil)
	}
	return nil
}

// HasStar reports whether the user has starred the repo. Used by the
// star button on the repo page to render the toggled state.
func HasStar(ctx context.Context, deps Deps, userID, repoID int64) (bool, error) {
	if userID == 0 {
		return false, nil
	}
	return socialdb.New().HasStar(ctx, deps.Pool, socialdb.HasStarParams{
		UserID: userID, RepoID: repoID,
	})
}

func hitStarLimit(ctx context.Context, deps Deps, userID int64) error {
	if deps.Limiter == nil {
		return nil
	}
	if err := deps.Limiter.Hit(ctx, deps.Pool, throttle.Limit{
		Scope:      "star",
		Identifier: fmt.Sprintf("user:%d", userID),
		Max:        starRateMax,
		Window:     starRateWindow,
	}); err != nil {
		return ErrStarRateLimit
	}
	return nil
}
