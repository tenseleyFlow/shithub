// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics_test

// PRO-EXT01-17: the campaign-wrap counter wires
// entitlements.CheckPrincipalFeature → shithub_pro_gate_total. This
// test pins the wiring: a successful CheckPrincipalFeature against a
// known feature must bump the counter with the right outcome label.
// A regression that detaches the SetObserveGate hook would otherwise
// silently zero out the entire soak signal PRO-EXT01-17 depends on.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	dto "github.com/prometheus/client_model/go"

	"github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const proGateFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestProGateTotal_FreeUserDecisionRecordsDenyOutcome(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "gate-free", DisplayName: "gate-free", PasswordHash: proGateFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	feature := entitlements.FeatureUserActionsSecrets
	before := readGateCounter(t, string(feature), "user", "deny")

	dec, err := entitlements.CheckPrincipalFeature(context.Background(),
		entitlements.Deps{Pool: pool}, billing.PrincipalForUser(u.ID), feature)
	if err != nil {
		t.Fatalf("CheckPrincipalFeature: %v", err)
	}
	if dec.Allowed {
		t.Fatalf("free user should be denied %s", feature)
	}
	if after := readGateCounter(t, string(feature), "user", "deny"); after-before < 1 {
		t.Errorf("deny counter did not advance: before=%v after=%v", before, after)
	}
}

func TestProGateTotal_ProUserDecisionRecordsAllowOutcome(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "gate-pro", DisplayName: "gate-pro", PasswordHash: proGateFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	now := time.Now().UTC()
	if _, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               u.ID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_pro_gate_test", Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_pro_gate_test",
	}); err != nil {
		t.Fatalf("upgrade: %v", err)
	}

	feature := entitlements.FeatureUserActionsSecrets
	before := readGateCounter(t, string(feature), "user", "allow")

	dec, err := entitlements.CheckPrincipalFeature(context.Background(),
		entitlements.Deps{Pool: pool}, billing.PrincipalForUser(u.ID), feature)
	if err != nil {
		t.Fatalf("CheckPrincipalFeature: %v", err)
	}
	if !dec.Allowed {
		t.Fatalf("pro user should be allowed %s; reason=%s", feature, dec.Reason)
	}
	if after := readGateCounter(t, string(feature), "user", "allow"); after-before < 1 {
		t.Errorf("allow counter did not advance: before=%v after=%v", before, after)
	}
}

// TestProGateTotal_EveryUserFeatureBumpsCounter sweeps every
// user-applicable Feature constant and asserts the gate counter
// advances. PRO-EXT_SR2-14: pre-fix only FeatureUserActionsSecrets
// was covered, so a regression that detached the observer hook for
// a subset of features would soak silently. The sweep catches that.
func TestProGateTotal_EveryUserFeatureBumpsCounter(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	u, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: "gate-sweep-free", DisplayName: "gate-sweep-free", PasswordHash: proGateFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	features := entitlements.FeaturesForKind(billing.SubjectKindUser)
	if len(features) == 0 {
		t.Fatal("FeaturesForKind(user) returned no features — registry empty?")
	}
	for _, f := range features {
		f := f
		t.Run(string(f), func(t *testing.T) {
			before := readGateCounter(t, string(f), "user", "deny")
			if _, err := entitlements.CheckPrincipalFeature(context.Background(),
				entitlements.Deps{Pool: pool}, billing.PrincipalForUser(u.ID), f); err != nil {
				t.Fatalf("CheckPrincipalFeature(%s): %v", f, err)
			}
			if after := readGateCounter(t, string(f), "user", "deny"); after-before < 1 {
				t.Errorf("deny counter did not advance for %s: before=%v after=%v", f, before, after)
			}
		})
	}
}

func readGateCounter(t *testing.T, feature, kind, outcome string) float64 {
	t.Helper()
	c, err := metrics.ProGateTotal.GetMetricWithLabelValues(feature, kind, outcome)
	if err != nil {
		t.Fatalf("GetMetricWithLabelValues: %v", err)
	}
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	return m.GetCounter().GetValue()
}
