// SPDX-License-Identifier: AGPL-3.0-or-later

package policy_test

// PRO-EXT01-15: paused-repo gate. The matrix test in policy_test.go is
// archive-and-deletion-focused; this file is the dedicated coverage
// for the new DenyPaused branch.

import (
	"context"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// TestPaused_WritesDenied: every write action on a paused repo
// (public or private, owner or anon) returns DenyPaused. Reads stay
// allowed for non-private viewers — the gate exists to discourage
// writes, not to hide the repo.
func TestPaused_WritesDenied(t *testing.T) {
	t.Parallel()
	d := policy.Deps{}
	ctx := context.Background()

	pausedRepo := policy.RepoRef{
		ID: 100, OwnerUserID: 7, Visibility: "public", IsPaused: true,
	}
	owner := policy.UserActor(7, "owner", false, false)

	writes := []policy.Action{
		policy.ActionRepoWrite,
		policy.ActionRepoAdmin,
		policy.ActionRepoSettingsGeneral,
		policy.ActionRepoSettingsBranches,
		policy.ActionPullCreate,
		policy.ActionPullMerge,
		policy.ActionIssueClose,
		policy.ActionForkCreate,
	}
	for _, action := range writes {
		dec := policy.Can(ctx, d, owner, action, pausedRepo)
		if dec.Allow {
			t.Errorf("owner write %s on paused repo allowed; expected deny", action)
		}
		if dec.Code != policy.DenyPaused {
			t.Errorf("owner write %s on paused repo: code=%v, want DenyPaused", action, dec.Code)
		}
	}
}

// TestPaused_ReadsAllowed: paused doesn't change visibility. A read
// against a paused public repo from an anonymous viewer still
// returns allow. Mirrors the archive matrix's read-still-works rule.
func TestPaused_ReadsAllowed(t *testing.T) {
	t.Parallel()
	d := policy.Deps{}
	ctx := context.Background()

	pausedPublic := policy.RepoRef{
		ID: 100, OwnerUserID: 7, Visibility: "public", IsPaused: true,
	}
	anon := policy.AnonymousActor()
	dec := policy.Can(ctx, d, anon, policy.ActionRepoRead, pausedPublic)
	if !dec.Allow {
		t.Errorf("anon read on paused public repo denied (%v); expected allow", dec)
	}
}

// TestPaused_PublicIssueParticipationDenied: a logged-in non-collab
// trying to open an issue on a paused public repo gets DenyPaused
// from the special public-issue-participation branch.
func TestPaused_PublicIssueParticipationDenied(t *testing.T) {
	t.Parallel()
	d := policy.Deps{}
	ctx := context.Background()

	pausedPublic := policy.RepoRef{
		ID: 100, OwnerUserID: 7, Visibility: "public", IsPaused: true,
	}
	stranger := policy.UserActor(42, "stranger", false, false)
	dec := policy.Can(ctx, d, stranger, policy.ActionIssueCreate, pausedPublic)
	if dec.Allow {
		t.Errorf("stranger issue-create on paused public repo allowed; want deny")
	}
	if dec.Code != policy.DenyPaused {
		t.Errorf("stranger issue-create on paused public repo: code=%v, want DenyPaused", dec.Code)
	}
}

// TestPaused_DeletedTrumps: a deleted-then-paused repo (shouldn't
// happen in practice but is defensible) still returns DenyRepoDeleted
// — deletion is the strongest signal and short-circuits everything.
func TestPaused_DeletedTrumps(t *testing.T) {
	t.Parallel()
	d := policy.Deps{}
	ctx := context.Background()

	pausedAndDeleted := policy.RepoRef{
		ID: 100, OwnerUserID: 7, Visibility: "public",
		IsPaused: true, IsDeleted: true,
	}
	owner := policy.UserActor(7, "owner", false, false)
	dec := policy.Can(ctx, d, owner, policy.ActionRepoRead, pausedAndDeleted)
	if dec.Code != policy.DenyRepoDeleted {
		t.Errorf("deleted+paused: code=%v, want DenyRepoDeleted", dec.Code)
	}
}
