// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// TestProfilePinsCapConstantsAreDefinedForBothPlans locks the PRO01
// ratification: LimitProfilePinsFreeCap = 6, LimitProfilePinsProCap = 100.
// Both limits report Defined=true regardless of the principal's plan
// — the cap is a constant, the *applicable* cap is whichever matches
// CanUse(FeatureProfilePinsBeyondFree).
func TestProfilePinsCapConstantsAreDefinedForBothPlans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, userID := setupEntitlementUser(t, "freepin")

	set, err := entitlements.ForPrincipal(ctx, entitlements.Deps{Pool: pool}, billing.PrincipalForUser(userID))
	if err != nil {
		t.Fatalf("ForPrincipal: %v", err)
	}
	free, err := set.Limit(entitlements.LimitProfilePinsFreeCap)
	if err != nil {
		t.Fatalf("Limit free: %v", err)
	}
	if !free.Defined || free.Value != entitlements.FreeProfilePinsCap {
		t.Errorf("LimitProfilePinsFreeCap = %+v, want defined value 6", free)
	}
	pro, err := set.Limit(entitlements.LimitProfilePinsProCap)
	if err != nil {
		t.Fatalf("Limit pro: %v", err)
	}
	if !pro.Defined || pro.Value != entitlements.ProProfilePinsCap {
		t.Errorf("LimitProfilePinsProCap = %+v, want defined value 100", pro)
	}
}

func TestProfilePinsBeyondFreeFreeUserCannotUseFeature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, userID := setupEntitlementUser(t, "freecap")
	set, err := entitlements.ForPrincipal(ctx, entitlements.Deps{Pool: pool}, billing.PrincipalForUser(userID))
	if err != nil {
		t.Fatalf("ForPrincipal: %v", err)
	}
	decision := set.CanUse(entitlements.FeatureProfilePinsBeyondFree)
	if decision.Allowed {
		t.Errorf("Free user should NOT be allowed FeatureProfilePinsBeyondFree, got %+v", decision)
	}
}

func TestProfilePinsBeyondFreeProUserCanUseFeature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, userID := setupEntitlementUser(t, "prouser")
	now := time.Now().UTC()
	if err := upgradeUserToPro(ctx, pool, userID, now); err != nil {
		t.Fatalf("upgrade to pro: %v", err)
	}

	set, err := entitlements.ForPrincipal(ctx, entitlements.Deps{
		Pool: pool,
		Now:  func() time.Time { return now },
	}, billing.PrincipalForUser(userID))
	if err != nil {
		t.Fatalf("ForPrincipal: %v", err)
	}
	decision := set.CanUse(entitlements.FeatureProfilePinsBeyondFree)
	if !decision.Allowed {
		t.Errorf("Pro user should be allowed FeatureProfilePinsBeyondFree, got %+v", decision)
	}
}

func setupEntitlementUser(t *testing.T, username string) (*pgxpool.Pool, int64) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	user, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return pool, user.ID
}

func upgradeUserToPro(ctx context.Context, pool *pgxpool.Pool, userID int64, now time.Time) error {
	suffix := strconv.FormatInt(userID, 10)
	_, err := billingdb.New().ApplyUserSubscriptionSnapshot(ctx, pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_pro_" + suffix,
	})
	return err
}
