// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestSettingsBranchesBlocksRequiredReviewersWithoutEntitlement(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-org-repo", reposdb.RepoVisibilityPrivate)
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/acme/private-org-repo/settings/branches", url.Values{
		"pattern":               {"trunk"},
		"required_review_count": {"2"},
	})
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	// PRO08 C3: count=2 is multi-reviewer, so the deny carries the
	// multi-reviewer-specific code. The single-reviewer code is only
	// used when count==1.
	if got := resp.Header().Get("Location"); got != "/acme/private-org-repo/settings/branches?notice=required-reviewers-multi-upgrade" {
		t.Fatalf("redirect location=%q", got)
	}
	assertBranchProtectionRuleCount(t, f, repo.ID, 0)
}

func TestSettingsBranchesBlocksAdvancedChecksWithoutEntitlement(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-org-repo", reposdb.RepoVisibilityPrivate)
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/acme/private-org-repo/settings/branches", url.Values{
		"pattern":                             {"trunk"},
		"required_status_check_names":         {"ci, lint"},
		"dismiss_stale_status_checks_on_push": {"on"},
	})
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/acme/private-org-repo/settings/branches?notice=branch-protection-upgrade" {
		t.Fatalf("redirect location=%q", got)
	}
	assertBranchProtectionRuleCount(t, f, repo.ID, 0)
}

func TestSettingsBranchesAllowsPublicOrgRepoAdvancedSettingsOnFreePlan(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "public-org-repo", reposdb.RepoVisibilityPublic)
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/acme/public-org-repo/settings/branches", url.Values{
		"pattern":                             {"trunk"},
		"required_review_count":               {"1"},
		"dismiss_stale_reviews_on_push":       {"on"},
		"required_status_check_names":         {"ci"},
		"dismiss_stale_status_checks_on_push": {"on"},
	})
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/acme/public-org-repo/settings/branches?notice=saved" {
		t.Fatalf("redirect location=%q", got)
	}
	assertBranchProtectionRule(t, f, repo.ID, 1, []string{"ci"}, true, true)
}

func TestSettingsBranchesAllowsPaidPrivateOrgRepoAdvancedSettings(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-org-repo", reposdb.RepoVisibilityPrivate)
	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)

	now := time.Now().UTC()
	if _, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: f.pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_branches_test",
		StripeSubscriptionItemID: "si_branches_test",
		CurrentPeriodStart:       now.Add(-time.Hour),
		CurrentPeriodEnd:         now.Add(24 * time.Hour),
		LastWebhookEventID:       "evt_branches_test",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/acme/private-org-repo/settings/branches", url.Values{
		"pattern":                             {"trunk"},
		"required_review_count":               {"2"},
		"dismiss_stale_reviews_on_push":       {"on"},
		"required_status_check_names":         {"ci, lint"},
		"dismiss_stale_status_checks_on_push": {"on"},
	})
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/acme/private-org-repo/settings/branches?notice=saved" {
		t.Fatalf("redirect location=%q", got)
	}
	assertBranchProtectionRule(t, f, repo.ID, 2, []string{"ci", "lint"}, true, true)
}

func TestSettingsBranchesAllowsDowngradedOrgToRemoveAdvancedSettings(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-org-repo", reposdb.RepoVisibilityPrivate)
	ruleID, err := f.handlers.rq.UpsertBranchProtectionRule(context.Background(), f.pool, reposdb.UpsertBranchProtectionRuleParams{
		RepoID:               repo.ID,
		Pattern:              "trunk",
		PreventDeletion:      true,
		AllowedPusherUserIds: []int64{},
	})
	if err != nil {
		t.Fatalf("seed branch rule: %v", err)
	}
	if err := f.handlers.rq.UpdateBranchProtectionReviewSettings(context.Background(), f.pool, reposdb.UpdateBranchProtectionReviewSettingsParams{
		ID:                        ruleID,
		RequiredReviewCount:       2,
		DismissStaleReviewsOnPush: true,
	}); err != nil {
		t.Fatalf("seed review settings: %v", err)
	}
	if err := f.handlers.rq.UpdateBranchProtectionCheckSettings(context.Background(), f.pool, reposdb.UpdateBranchProtectionCheckSettingsParams{
		ID:                             ruleID,
		StatusChecksRequired:           []string{"ci"},
		DismissStaleStatusChecksOnPush: true,
	}); err != nil {
		t.Fatalf("seed check settings: %v", err)
	}

	mux := f.branchesSettingsMux(f.owner.ID, f.owner.Username)
	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/acme/private-org-repo/settings/branches", url.Values{
		"id":      {strconv.FormatInt(ruleID, 10)},
		"pattern": {"trunk"},
	})
	mux.ServeHTTP(resp, req)

	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/acme/private-org-repo/settings/branches?notice=saved" {
		t.Fatalf("redirect location=%q", got)
	}
	assertBranchProtectionRule(t, f, repo.ID, 0, nil, false, false)
}

func (f *repoFixture) branchesSettingsMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	f.handlers.MountSettingsBranches(mux)
	return mux
}

func (f *repoFixture) insertOrgRepo(t *testing.T, orgID int64, name string, visibility reposdb.RepoVisibility) reposdb.Repo {
	t.Helper()
	repo, err := f.handlers.rq.CreateRepo(context.Background(), f.pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          name,
		Description:   "",
		Visibility:    visibility,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo org-owned: %v", err)
	}
	return repo
}

func assertBranchProtectionRuleCount(t *testing.T, f *repoFixture, repoID int64, want int) {
	t.Helper()
	rules, err := f.handlers.rq.ListBranchProtectionRules(context.Background(), f.pool, repoID)
	if err != nil {
		t.Fatalf("ListBranchProtectionRules: %v", err)
	}
	if got := len(rules); got != want {
		t.Fatalf("branch protection rule count=%d want=%d", got, want)
	}
}

func assertBranchProtectionRule(t *testing.T, f *repoFixture, repoID int64, wantReviews int32, wantChecks []string, wantDismissReviews, wantDismissChecks bool) {
	t.Helper()
	rules, err := f.handlers.rq.ListBranchProtectionRules(context.Background(), f.pool, repoID)
	if err != nil {
		t.Fatalf("ListBranchProtectionRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("branch protection rule count=%d want=1", len(rules))
	}
	rule := rules[0]
	if rule.RequiredReviewCount != wantReviews {
		t.Fatalf("required review count=%d want=%d", rule.RequiredReviewCount, wantReviews)
	}
	if len(rule.StatusChecksRequired) != len(wantChecks) {
		t.Fatalf("status checks=%v want=%v", rule.StatusChecksRequired, wantChecks)
	}
	for i := range wantChecks {
		if rule.StatusChecksRequired[i] != wantChecks[i] {
			t.Fatalf("status checks=%v want=%v", rule.StatusChecksRequired, wantChecks)
		}
	}
	if rule.DismissStaleReviewsOnPush != wantDismissReviews {
		t.Fatalf("dismiss stale reviews=%v want=%v", rule.DismissStaleReviewsOnPush, wantDismissReviews)
	}
	if rule.DismissStaleStatusChecksOnPush != wantDismissChecks {
		t.Fatalf("dismiss stale checks=%v want=%v", rule.DismissStaleStatusChecksOnPush, wantDismissChecks)
	}
}
