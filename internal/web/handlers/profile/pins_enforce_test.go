// SPDX-License-Identifier: AGPL-3.0-or-later

package profile_test

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

// PRO07 — profile pin cap enforcement.
//
// The cap is resolved per-principal via entitlements (Free=6, Pro=100).
// Operators flip BillingEnforce.UserProfilePinsBeyondFree to make a
// Free user's 7th-pin attempt return 402 with an upgrade banner.
// Otherwise the gate is report-only: mirrors the BP gate by letting
// the save proceed (up to the Pro cap) and emitting an
// entitlements.report_only_deny log when the user crosses the Free
// cap. F-shakedown corrected the prior behavior, which had blocked
// the save with 400 even in report-only.

func TestProfilePins_FreeUserBlockedAtCapWithEnforce(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserProfilePinsBeyondFree: true,
	})
	alice := env.insertUser(t, "freealice", "Alice", "")
	repos := makeUserPinCandidates(t, env, alice.ID, 7)

	resp := env.postPins(t, "/freealice/pins", alice, repos...)
	if resp.StatusCode != http.StatusPaymentRequired {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Free user 7th pin: status=%d, want 402; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Pinned repositories") {
		t.Errorf("upgrade banner missing feature label: %s", body)
	}
}

func TestProfilePins_FreeUserReportOnlyWithoutEnforce(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserProfilePinsBeyondFree: false,
	})
	alice := env.insertUser(t, "softalice", "Alice", "")
	repos := makeUserPinCandidates(t, env, alice.ID, 7)

	resp := env.postPins(t, "/softalice/pins", alice, repos...)
	// Report-only mirrors the BP gate: the over-Free-cap submission
	// saves successfully and the would-deny lands in the logger.
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("report-only over-cap status=%d, want 303; body=%s", resp.StatusCode, body)
	}
}

func TestProfilePins_FreeUserReportOnlyOverProCap(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserProfilePinsBeyondFree: false,
	})
	alice := env.insertUser(t, "noisyalice", "Alice", "")
	repos := makeUserPinCandidates(t, env, alice.ID, int(entitlements.ProProfilePinsCap)+1)

	resp := env.postPins(t, "/noisyalice/pins", alice, repos...)
	// Report-only lifts the effective cap to Pro=100; submissions
	// above that are a DB-sanity 400, not an upgrade prompt.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("report-only over-Pro-cap status=%d, want 400", resp.StatusCode)
	}
}

func TestProfilePins_FreeUserUnderCapSavesWithEnforce(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserProfilePinsBeyondFree: true,
	})
	alice := env.insertUser(t, "fitalice", "Alice", "")
	repos := makeUserPinCandidates(t, env, alice.ID, int(entitlements.FreeProfilePinsCap))

	resp := env.postPins(t, "/fitalice/pins", alice, repos...)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Free user at-cap save: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestProfilePins_ProUserCanExceedFreeCap(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserProfilePinsBeyondFree: true,
	})
	alice := env.insertUser(t, "proalice", "Alice", "")
	upgradeUserToProForTest(t, env, alice.ID)
	repos := makeUserPinCandidates(t, env, alice.ID, int(entitlements.FreeProfilePinsCap)+3)

	resp := env.postPins(t, "/proalice/pins", alice, repos...)
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Pro user over-Free-cap save: status=%d body=%s", resp.StatusCode, body)
	}
}

func TestProfilePins_ProUserBlockedAboveProCap(t *testing.T) {
	t.Parallel()
	env := setupProfileEnvWithBillingEnforce(t, config.EnforceConfig{
		UserProfilePinsBeyondFree: true,
	})
	alice := env.insertUser(t, "manyalice", "Alice", "")
	upgradeUserToProForTest(t, env, alice.ID)
	repos := makeUserPinCandidates(t, env, alice.ID, int(entitlements.ProProfilePinsCap)+1)

	resp := env.postPins(t, "/manyalice/pins", alice, repos...)
	// Pro user over the Pro cap is a DB-sanity 400, NOT a 402 — the
	// upgrade banner doesn't help here.
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("Pro user >100 pins: status=%d, want 400", resp.StatusCode)
	}
}

// makeUserPinCandidates inserts `count` public user-owned repos and
// returns their IDs. Names are r0, r1, ... so the test can keep them
// distinct without colliding with any other fixture.
func makeUserPinCandidates(t *testing.T, env *profileEnv, userID int64, count int) []int64 {
	t.Helper()
	out := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		id := env.insertUserRepo(t, userID, "r"+strconv.Itoa(i), "pin", "public", "Go", 0, 0)
		out = append(out, id)
	}
	return out
}

func upgradeUserToProForTest(t *testing.T, env *profileEnv, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(userID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), env.pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_pin_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_pin_pro_" + suffix,
	})
	if err != nil {
		t.Fatalf("upgrade user to Pro: %v", err)
	}
}
