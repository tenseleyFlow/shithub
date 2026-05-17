// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

type apiUserPlan struct {
	Plan              string `json:"plan"`
	Status            string `json:"status"`
	CurrentPeriodEnd  any    `json:"current_period_end"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	GraceUntil        any    `json:"grace_until"`
	Features          map[string]struct {
		Allowed bool `json:"allowed"`
	} `json:"features"`
	Limits map[string]int64 `json:"limits"`
}

// TestUserPlan_FreeAccountReturnsLockedFeatures confirms a Free user
// gets a populated response (no 404), every gated user-tier feature
// returns allowed=false, and limits.profile_pins is the Free cap.
func TestUserPlan_FreeAccountReturnsLockedFeatures(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/plan", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp apiUserPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if resp.Plan != "free" {
		t.Errorf("plan: got %q, want free", resp.Plan)
	}
	if resp.Status != "none" {
		t.Errorf("status: got %q, want none", resp.Status)
	}
	if resp.CurrentPeriodEnd != nil {
		t.Errorf("current_period_end: got %v, want null", resp.CurrentPeriodEnd)
	}
	if resp.GraceUntil != nil {
		t.Errorf("grace_until: got %v, want null", resp.GraceUntil)
	}
	if resp.Limits["profile_pins"] != 6 {
		t.Errorf("limits.profile_pins: got %d, want 6", resp.Limits["profile_pins"])
	}
	for _, feature := range []string{"profile_pins_beyond_free", "required_reviewers", "advanced_branch_protection", "profile_vanity"} {
		entry, ok := resp.Features[feature]
		if !ok {
			t.Errorf("features.%s missing from response: %s", feature, rr.Body.String())
			continue
		}
		if entry.Allowed {
			t.Errorf("features.%s: got allowed=true, want false for Free account", feature)
		}
	}
}

// TestUserPlan_ProAccountUnlocksFeatures confirms an active Pro
// subscription flips every personal-Pro feature to allowed=true and
// raises limits.profile_pins to the Pro cap.
func TestUserPlan_ProAccountUnlocksFeatures(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	upgradeUserToActivePro(t, pool, userID)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/plan", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp apiUserPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if resp.Plan != "pro" {
		t.Errorf("plan: got %q, want pro", resp.Plan)
	}
	if resp.Status != "active" {
		t.Errorf("status: got %q, want active", resp.Status)
	}
	if resp.CurrentPeriodEnd == nil {
		t.Errorf("current_period_end: got null, want timestamp")
	}
	if resp.Limits["profile_pins"] != 100 {
		t.Errorf("limits.profile_pins: got %d, want 100", resp.Limits["profile_pins"])
	}
	for _, feature := range []string{"profile_pins_beyond_free", "required_reviewers", "advanced_branch_protection", "profile_vanity"} {
		entry, ok := resp.Features[feature]
		if !ok {
			t.Errorf("features.%s missing from response: %s", feature, rr.Body.String())
			continue
		}
		if !entry.Allowed {
			t.Errorf("features.%s: got allowed=false, want true for Pro account", feature)
		}
	}
}

// TestUserPlan_RequiresUserReadScope confirms a PAT that carries only
// a different scope is rejected with 403 + the canonical scope envelope.
func TestUserPlan_RequiresUserReadScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	// Wrong scope (repo:read) — RequireScope(user:read) denies.
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/plan", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if want := string(pat.ScopeUserRead); rr.Header().Get("X-Accepted-OAuth-Scopes") != want {
		t.Errorf("X-Accepted-OAuth-Scopes: got %q, want %q", rr.Header().Get("X-Accepted-OAuth-Scopes"), want)
	}
}

// TestUserPlan_Unauthenticated confirms an anonymous request is
// rejected with 401 + a JSON envelope.
func TestUserPlan_Unauthenticated(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/plan", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json prefix", ct)
	}
}

// upgradeUserToActivePro mirrors the helper in
// profile/pins_enforce_test.go: applies a Pro subscription snapshot
// against the user_billing_states row seeded by the AFTER INSERT
// trigger on users.
func upgradeUserToActivePro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(userID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_user_plan_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_user_plan_pro_" + suffix,
	})
	if err != nil {
		t.Fatalf("ApplyUserSubscriptionSnapshot: %v", err)
	}
}

// TestUserPlan_ContractEnumeratesEveryUserFeature is the trip-wire
// audit fix from PRO-EXT_SR2-09. The /api/v1/user/plan response
// promises CLIs a per-feature `allowed` flag for every gated
// user-tier feature — the PRO-EXT01 audit caught the response
// silently missing nine features added across the campaign. This
// test enumerates `entitlements.FeaturesForKind(SubjectKindUser)`
// and fails by name if any are absent from the response so future
// sprints can't ship a new Feature* without exposing it.
func TestUserPlan_ContractEnumeratesEveryUserFeature(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/plan", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var body apiUserPlan
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, f := range entitlements.FeaturesForKind(billing.SubjectKindUser) {
		if _, ok := body.Features[string(f)]; !ok {
			t.Errorf("user-applicable feature %q missing from /api/v1/user/plan response — "+
				"add it to userPlanFeatures in internal/web/handlers/api/user_plan.go", f)
		}
	}
}
