// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements_test

// PRO-EXT_SR2-14 (audit coverage gap): explicit Pro→Free downgrade
// tests for the features the audit called out as lacking transition
// coverage. The billing state lifecycle is well-tested upstream in
// internal/billing; this file pins the entitlement-decision side —
// after a MarkUserCanceled the gate must flip from allow → deny for
// every feature in the inventory.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const downgradeFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// TestDowngrade_UserProToFreeFlipsCalledOutFeatures exercises the
// canonical billing transition (active Pro → MarkUserCanceled → Free)
// and asserts the four features the audit flagged for missing
// downgrade coverage transition from Allowed=true → Allowed=false.
func TestDowngrade_UserProToFreeFlipsCalledOutFeatures(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()

	u, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "downgrade-user", DisplayName: "downgrade-user", PasswordHash: downgradeFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	now := time.Now().UTC()
	q := billingdb.New()
	if _, err := q.ApplyUserSubscriptionSnapshot(ctx, pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               u.ID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_downgrade_test", Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_downgrade_upgrade",
	}); err != nil {
		t.Fatalf("upgrade snapshot: %v", err)
	}

	features := []entitlements.Feature{
		entitlements.FeatureWebhookRelay,
		entitlements.FeatureCronWorkflowDispatch,
		entitlements.FeatureInboxDigests,
		entitlements.FeatureRepoTimeMachine,
	}

	principal := billing.PrincipalForUser(u.ID)

	// Pre-downgrade: every feature must be allowed for an active Pro.
	for _, f := range features {
		dec, err := entitlements.CheckPrincipalFeature(ctx, entitlements.Deps{Pool: pool}, principal, f)
		if err != nil {
			t.Fatalf("CheckPrincipalFeature(%s) pre-downgrade: %v", f, err)
		}
		if !dec.Allowed {
			t.Errorf("Pro user must be allowed %s pre-downgrade; reason=%s", f, dec.Reason)
		}
	}

	if _, err := q.MarkUserCanceled(ctx, pool, billingdb.MarkUserCanceledParams{
		UserID:             u.ID,
		LastWebhookEventID: "evt_downgrade_cancel",
	}); err != nil {
		t.Fatalf("MarkUserCanceled: %v", err)
	}

	// Post-downgrade: every feature must now be denied.
	for _, f := range features {
		dec, err := entitlements.CheckPrincipalFeature(ctx, entitlements.Deps{Pool: pool}, principal, f)
		if err != nil {
			t.Fatalf("CheckPrincipalFeature(%s) post-downgrade: %v", f, err)
		}
		if dec.Allowed {
			t.Errorf("Free user must be denied %s post-downgrade; got Allowed=true", f)
		}
		if string(dec.RequiredPlan) != "pro" {
			t.Errorf("post-downgrade decision for %s should advise RequiredPlan=pro; got %q", f, dec.RequiredPlan)
		}
	}
}
