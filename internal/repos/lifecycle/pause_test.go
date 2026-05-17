// SPDX-License-Identifier: AGPL-3.0-or-later

package lifecycle_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos/lifecycle"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// TestPauseUnpause is the happy-path table: pause stamps the row,
// idempotent re-pause returns ErrAlreadyPaused, unpause clears it,
// double unpause returns ErrNotPaused. Mirrors TestArchiveUnarchive.
func TestPauseUnpause(t *testing.T) {
	t.Parallel()
	env := setup(t)
	ctx := context.Background()

	if err := lifecycle.Pause(ctx, env.deps, env.alice.ID, env.repoID, ""); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := lifecycle.Pause(ctx, env.deps, env.alice.ID, env.repoID, ""); !errors.Is(err, lifecycle.ErrAlreadyPaused) {
		t.Errorf("double pause: err=%v, want ErrAlreadyPaused", err)
	}
	// Reload + sanity check the columns flipped.
	repo, err := reposdb.New().GetRepoByID(ctx, env.deps.Pool, env.repoID)
	if err != nil {
		t.Fatalf("GetRepoByID: %v", err)
	}
	if !repo.IsPaused {
		t.Errorf("is_paused = false, want true")
	}
	if !repo.PausedAt.Valid {
		t.Errorf("paused_at not set after Pause")
	}

	if err := lifecycle.Unpause(ctx, env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("Unpause: %v", err)
	}
	if err := lifecycle.Unpause(ctx, env.deps, env.alice.ID, env.repoID); !errors.Is(err, lifecycle.ErrNotPaused) {
		t.Errorf("double unpause: err=%v, want ErrNotPaused", err)
	}
	repo, _ = reposdb.New().GetRepoByID(ctx, env.deps.Pool, env.repoID)
	if repo.IsPaused {
		t.Errorf("is_paused still true after Unpause")
	}
	if repo.PausedAt.Valid {
		t.Errorf("paused_at still set after Unpause")
	}
}

// TestPause_ArchivedRejected: archive and pause are mutually exclusive.
// Pause on an archived repo must surface ErrCannotPauseArchived without
// touching the DB.
func TestPause_ArchivedRejected(t *testing.T) {
	t.Parallel()
	env := setup(t)
	ctx := context.Background()

	if err := lifecycle.Archive(ctx, env.deps, env.alice.ID, env.repoID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	err := lifecycle.Pause(ctx, env.deps, env.alice.ID, env.repoID, "stop")
	if !errors.Is(err, lifecycle.ErrCannotPauseArchived) {
		t.Errorf("pause on archived: err=%v, want ErrCannotPauseArchived", err)
	}
	repo, _ := reposdb.New().GetRepoByID(ctx, env.deps.Pool, env.repoID)
	if repo.IsPaused {
		t.Errorf("is_paused = true; archived repo must not have been paused")
	}
}

// TestPause_ReasonStored confirms the optional pause_reason is persisted
// when supplied and trimmed/truncated per the contract.
func TestPause_ReasonStored(t *testing.T) {
	t.Parallel()
	env := setup(t)
	ctx := context.Background()

	const reason = "winter break — back in March"
	if err := lifecycle.Pause(ctx, env.deps, env.alice.ID, env.repoID, "  "+reason+"  "); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	repo, _ := reposdb.New().GetRepoByID(ctx, env.deps.Pool, env.repoID)
	if !repo.PauseReason.Valid || repo.PauseReason.String != reason {
		t.Errorf("pause_reason = %v, want %q (trimmed)", repo.PauseReason, reason)
	}
}

// TestPause_ReasonTruncated: an over-long reason is silently chopped.
// The DB constraint is at PauseReasonMaxLen; lifecycle truncates first
// so a programmatic caller doesn't see the raw SQL violation.
func TestPause_ReasonTruncated(t *testing.T) {
	t.Parallel()
	env := setup(t)
	ctx := context.Background()

	long := strings.Repeat("a", lifecycle.PauseReasonMaxLen+50)
	if err := lifecycle.Pause(ctx, env.deps, env.alice.ID, env.repoID, long); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	repo, _ := reposdb.New().GetRepoByID(ctx, env.deps.Pool, env.repoID)
	if len(repo.PauseReason.String) != lifecycle.PauseReasonMaxLen {
		t.Errorf("len(pause_reason) = %d, want %d", len(repo.PauseReason.String), lifecycle.PauseReasonMaxLen)
	}
}
