// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// PauseReasonMaxLen mirrors the DB check constraint. Surfaced as a
// constant so handlers can validate before round-tripping (gives a
// nicer error than the pgx constraint-violation path).
const PauseReasonMaxLen = 280

// Pause flips is_paused=true on the repo (PRO-EXT01-15). Idempotent
// in spirit — a double-pause returns ErrAlreadyPaused so handlers can
// surface a friendly "this repo is already paused" message. Paired
// with Unpause; archive and pause are DB-level mutually exclusive
// (constraint repos_pause_archive_mutex), so we surface that
// up-front as ErrCannotPauseArchived rather than the raw SQL error.
//
// reason is optional and capped at PauseReasonMaxLen runes; an
// over-long reason is silently truncated rather than rejected — the
// handler validates user input before reaching here, but truncation
// is the safer default if a programmatic caller forgets.
func Pause(ctx context.Context, deps Deps, actorUserID, repoID int64, reason string) error {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if repo.IsArchived {
		return ErrCannotPauseArchived
	}
	if repo.IsPaused {
		return ErrAlreadyPaused
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > PauseReasonMaxLen {
		reason = reason[:PauseReasonMaxLen]
	}
	pauseReason := pgtype.Text{}
	if reason != "" {
		pauseReason = pgtype.Text{String: reason, Valid: true}
	}
	if err := rq.PauseRepo(ctx, deps.Pool, reposdb.PauseRepoParams{
		ID:          repoID,
		PauseReason: pauseReason,
	}); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoPaused, audit.TargetRepo, repoID, nil)
	}
	return nil
}

// Unpause clears is_paused. Same idempotency contract as Pause.
func Unpause(ctx context.Context, deps Deps, actorUserID, repoID int64) error {
	rq := reposdb.New()
	repo, err := rq.GetRepoByID(ctx, deps.Pool, repoID)
	if err != nil {
		return fmt.Errorf("load repo: %w", err)
	}
	if !repo.IsPaused {
		return ErrNotPaused
	}
	if err := rq.UnpauseRepo(ctx, deps.Pool, repoID); err != nil {
		return fmt.Errorf("unpause: %w", err)
	}
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionRepoUnpaused, audit.TargetRepo, repoID, nil)
	}
	return nil
}
