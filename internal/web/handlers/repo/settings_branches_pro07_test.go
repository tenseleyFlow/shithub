// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// PRO07 — user-kind branch protection enforce flip.
//
// PRO05 plumbed user-kind report-only logging through
// evaluateBranchProtectionFeature. PRO07 wires
// EnforceConfig.User{RequiredReviewers,AdvancedBranchProtection} into
// the same site so operators can flip per-feature enforcement after
// the telemetry soak. These tests pin both modes.

func TestSettingsBranches_UserKindReportOnlyRequiredReviewersAllowsSave(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t) // zero-value enforce ⇒ report-only
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest("POST", "/alice/private-repo/settings/branches", url.Values{
		"pattern":               {"trunk"},
		"required_review_count": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != 303 {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	// Report-only ⇒ no notice code; redirect lands on the saved
	// confirmation page.
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/branches?notice=saved" {
		t.Fatalf("expected report-only save, got redirect=%q", got)
	}
	assertBranchProtectionRuleCount(t, f, f.privateRepo.ID, 1)
}

func TestSettingsBranches_UserKindEnforceFlipRequiredReviewersBlocksSingle(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{
		UserRequiredReviewers: true,
	})
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest("POST", "/alice/private-repo/settings/branches", url.Values{
		"pattern":               {"trunk"},
		"required_review_count": {"1"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != 303 {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	// PRO08 C3 + C4: count=1 + user kind → single-reviewer Pro copy.
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/branches?notice=required-reviewers-upgrade-pro" {
		t.Fatalf("expected single-reviewer Pro notice, got %q", got)
	}
	assertBranchProtectionRuleCount(t, f, f.privateRepo.ID, 0)
}

// TestSettingsBranches_UserKindEnforceFlipMultiReviewerBlocks locks
// PRO08 C3's distinct copy variant when count > 1.
func TestSettingsBranches_UserKindEnforceFlipMultiReviewerBlocks(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{
		UserRequiredReviewers: true,
	})
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest("POST", "/alice/private-repo/settings/branches", url.Values{
		"pattern":               {"trunk"},
		"required_review_count": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != 303 {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/branches?notice=required-reviewers-multi-upgrade-pro" {
		t.Fatalf("expected multi-reviewer Pro notice, got %q", got)
	}
	assertBranchProtectionRuleCount(t, f, f.privateRepo.ID, 0)
}

func TestSettingsBranches_UserKindEnforceFlipAdvancedChecksBlocks(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{
		UserAdvancedBranchProtection: true,
	})
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest("POST", "/alice/private-repo/settings/branches", url.Values{
		"pattern":                     {"trunk"},
		"required_status_check_names": {"ci"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != 303 {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	// PRO08 C4: user-kind deny carries the Pro-flavored upgrade copy.
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/branches?notice=branch-protection-upgrade-pro" {
		t.Fatalf("expected Pro upgrade notice, got %q", got)
	}
	assertBranchProtectionRuleCount(t, f, f.privateRepo.ID, 0)
}

// TestSettingsBranches_UserKindEnforceFlipPreventForcePushBlocks locks
// PRO08 C2: the rewired gate fires on prevent_force_push (NOT just
// required_status_checks like PRO07 mis-wired it). prevent_deletion
// and require_signed_commits behave identically; covered by table.
func TestSettingsBranches_UserKindEnforceFlipPreventForcePushBlocks(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"prevent_force_push", "prevent_deletion", "require_signed_commits"} {
		flag := flag
		t.Run(flag, func(t *testing.T) {
			t.Parallel()
			f := newRepoFixtureWithEnforce(t, config.EnforceConfig{
				UserAdvancedBranchProtection: true,
			})
			mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)
			resp := httptest.NewRecorder()
			req := newFormRequest("POST", "/alice/private-repo/settings/branches", url.Values{
				"pattern": {"trunk"},
				flag:      {"on"},
			})
			mux.ServeHTTP(resp, req)
			if resp.Code != 303 {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/branches?notice=branch-protection-upgrade-pro" {
				t.Fatalf("flag=%s redirect=%q, want Pro upgrade", flag, got)
			}
			assertBranchProtectionRuleCount(t, f, f.privateRepo.ID, 0)
		})
	}
}

// TestSettingsBranches_UserKindEnforceFlipDoesNotAffectPublicRepos
// locks the visibility short-circuit: enforce flags fire only on
// private personal repos. Public personal repos stay ungated.
func TestSettingsBranches_UserKindEnforceFlipDoesNotAffectPublicRepos(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{
		UserRequiredReviewers:        true,
		UserAdvancedBranchProtection: true,
	})
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest("POST", "/alice/public-repo/settings/branches", url.Values{
		"pattern":               {"trunk"},
		"required_review_count": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != 303 {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/public-repo/settings/branches?notice=saved" {
		t.Fatalf("public repo should bypass gate, got %q", got)
	}
}

// TestSettingsBranches_UserKindEnforceFlipAllowsProUser verifies the
// gate is keyed on Plan, not just kind: a Pro personal account
// completes the save even with the enforce flag flipped.
func TestSettingsBranches_UserKindEnforceFlipAllowsProUser(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{
		UserRequiredReviewers: true,
	})
	upgradeRepoFixtureOwnerToPro(t, f)
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest("POST", "/alice/private-repo/settings/branches", url.Values{
		"pattern":               {"trunk"},
		"required_review_count": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != 303 {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/private-repo/settings/branches?notice=saved" {
		t.Fatalf("Pro user with enforce should save, got %q", got)
	}
	assertBranchProtectionRuleCount(t, f, f.privateRepo.ID, 1)
}

// TestSettingsBranches_OrgKindUnchangedByPRO07Flags is the regression
// guard: PRO07's per-feature flags target user kind only. Org-kind
// gating has been on since SP05 and must not change shape when the
// user-kind flags flip.
func TestSettingsBranches_OrgKindUnchangedByPRO07Flags(t *testing.T) {
	t.Parallel()
	for _, enforce := range []config.EnforceConfig{
		{},
		{UserRequiredReviewers: true, UserAdvancedBranchProtection: true},
	} {
		f := newRepoFixtureWithEnforce(t, enforce)
		orgID := f.insertOwnedOrg(t, "acme")
		repo := f.insertOrgRepo(t, orgID, "private-org-repo", reposdb.RepoVisibilityPrivate)
		mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

		resp := httptest.NewRecorder()
		req := newFormRequest("POST", "/acme/private-org-repo/settings/branches", url.Values{
			"pattern":               {"trunk"},
			"required_review_count": {"2"},
		})
		mux.ServeHTTP(resp, req)
		if resp.Code != 303 {
			t.Fatalf("enforce=%+v status=%d body=%s", enforce, resp.Code, resp.Body.String())
		}
		// count=2 ⇒ multi; org-kind keeps the existing copy (no -pro
		// suffix); PRO08 C3 added the multi- variant.
		if got := resp.Header().Get("Location"); got != "/acme/private-org-repo/settings/branches?notice=required-reviewers-multi-upgrade" {
			t.Errorf("enforce=%+v: org redirect=%q, want upgrade notice", enforce, got)
		}
		assertBranchProtectionRuleCount(t, f, repo.ID, 0)
	}
}

func upgradeRepoFixtureOwnerToPro(t *testing.T, f *repoFixture) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(f.owner.ID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), f.pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               f.owner.ID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_bp_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_bp_pro_" + suffix,
	})
	if err != nil {
		t.Fatalf("upgrade owner to Pro: %v", err)
	}
}
