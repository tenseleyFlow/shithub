// SPDX-License-Identifier: AGPL-3.0-or-later

package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// setupUser mirrors `setup` but returns just the user — PRO03 user
// billing tests don't need an org. The schema's AFTER-INSERT trigger
// from 0073 seeds a `free` row in `user_billing_states` automatically.
func setupUser(t *testing.T, username string) (int64, *pgxpool.Pool) {
	t.Helper()
	p := dbtest.NewTestDB(t)
	u, err := usersdb.New().CreateUser(context.Background(), p, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID, p
}

func TestUserBillingState_SeedTriggerFires(t *testing.T) {
	uID, pool := setupUser(t, "alice")
	state, err := billingdb.New().GetUserBillingState(context.Background(), pool, uID)
	if err != nil {
		t.Fatalf("GetUserBillingState: %v", err)
	}
	if state.Plan != billingdb.UserPlanFree {
		t.Fatalf("seed plan: got %s, want free", state.Plan)
	}
	if state.SubscriptionStatus != billingdb.BillingSubscriptionStatusNone {
		t.Fatalf("seed status: got %s, want none", state.SubscriptionStatus)
	}
	if state.UserID != uID {
		t.Fatalf("seed user_id: got %d, want %d", state.UserID, uID)
	}
}

func TestUserBillingState_TransitionsFreeToProToPastDueToActiveToCanceled(t *testing.T) {
	uID, pool := setupUser(t, "alice")
	q := billingdb.New()
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Second)

	// free → pro (ApplyUserSubscriptionSnapshot active).
	applied, err := q.ApplyUserSubscriptionSnapshot(ctx, pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:                   uID,
		Plan:                     billingdb.UserPlanPro,
		SubscriptionStatus:       billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID:     pgtype.Text{String: "sub_user_test", Valid: true},
		StripeSubscriptionItemID: pgtype.Text{String: "si_user_test", Valid: true},
		CurrentPeriodStart:       pgtype.Timestamptz{Time: start, Valid: true},
		CurrentPeriodEnd:         pgtype.Timestamptz{Time: start.Add(30 * 24 * time.Hour), Valid: true},
		CancelAtPeriodEnd:        false,
		LastWebhookEventID:       "evt_user_active",
	})
	if err != nil {
		t.Fatalf("Apply free→pro: %v", err)
	}
	if applied.Plan != billingdb.UserPlanPro {
		t.Errorf("post-apply plan: got %s, want pro", applied.Plan)
	}
	if applied.SubscriptionStatus != billingdb.BillingSubscriptionStatusActive {
		t.Errorf("post-apply status: got %s, want active", applied.SubscriptionStatus)
	}
	// users.plan must mirror.
	user, _ := usersdb.New().GetUserByID(ctx, pool, uID)
	if user.Plan != usersdb.UserPlanPro {
		t.Errorf("users.plan after pro apply: got %s, want pro", user.Plan)
	}

	// pro → past_due (MarkUserPastDue returns UserBillingState — no CTE).
	pastDue, err := q.MarkUserPastDue(ctx, pool, billingdb.MarkUserPastDueParams{
		UserID:             uID,
		GraceUntil:         pgtype.Timestamptz{Time: start.Add(7 * 24 * time.Hour), Valid: true},
		LastWebhookEventID: "evt_user_past_due",
	})
	if err != nil {
		t.Fatalf("MarkUserPastDue: %v", err)
	}
	if pastDue.SubscriptionStatus != billingdb.BillingSubscriptionStatusPastDue {
		t.Errorf("past_due status: %s", pastDue.SubscriptionStatus)
	}
	if !pastDue.LockedAt.Valid {
		t.Errorf("past_due locked_at not set")
	}
	if pastDue.LockReason.BillingLockReason != billingdb.BillingLockReasonPastDue {
		t.Errorf("past_due lock_reason: %s", pastDue.LockReason.BillingLockReason)
	}

	// past_due → active (MarkUserPaymentSucceeded).
	recovered, err := q.MarkUserPaymentSucceeded(ctx, pool, billingdb.MarkUserPaymentSucceededParams{
		UserID:             uID,
		LastWebhookEventID: "evt_user_recovered",
	})
	if err != nil {
		t.Fatalf("MarkUserPaymentSucceeded: %v", err)
	}
	if recovered.SubscriptionStatus != billingdb.BillingSubscriptionStatusActive {
		t.Errorf("recovered status: %s", recovered.SubscriptionStatus)
	}
	if recovered.Plan != billingdb.UserPlanPro {
		t.Errorf("recovered plan: %s", recovered.Plan)
	}
	if recovered.LockedAt.Valid {
		t.Errorf("recovered locked_at: should be cleared")
	}

	// active → canceled (MarkUserCanceled).
	canceled, err := q.MarkUserCanceled(ctx, pool, billingdb.MarkUserCanceledParams{
		UserID:             uID,
		LastWebhookEventID: "evt_user_canceled",
	})
	if err != nil {
		t.Fatalf("MarkUserCanceled: %v", err)
	}
	if canceled.SubscriptionStatus != billingdb.BillingSubscriptionStatusCanceled {
		t.Errorf("canceled status: %s", canceled.SubscriptionStatus)
	}
	if canceled.Plan != billingdb.UserPlanFree {
		t.Errorf("canceled plan: got %s, want free", canceled.Plan)
	}
	user, _ = usersdb.New().GetUserByID(ctx, pool, uID)
	if user.Plan != usersdb.UserPlanFree {
		t.Errorf("users.plan after cancel: got %s, want free", user.Plan)
	}

	// canceled → unlocked + status none (ClearUserBillingLock).
	cleared, err := q.ClearUserBillingLock(ctx, pool, uID)
	if err != nil {
		t.Fatalf("ClearUserBillingLock: %v", err)
	}
	if cleared.SubscriptionStatus != billingdb.BillingSubscriptionStatusNone {
		t.Errorf("cleared status: got %s, want none", cleared.SubscriptionStatus)
	}
	if cleared.LockedAt.Valid {
		t.Errorf("cleared locked_at: should be NULL")
	}
}

func TestUserBillingState_SetStripeCustomer(t *testing.T) {
	uID, pool := setupUser(t, "alice")
	state, err := billingdb.New().SetUserStripeCustomer(context.Background(), pool, billingdb.SetUserStripeCustomerParams{
		UserID:           uID,
		StripeCustomerID: pgtype.Text{String: "cus_user_test", Valid: true},
	})
	if err != nil {
		t.Fatalf("SetUserStripeCustomer: %v", err)
	}
	if !state.StripeCustomerID.Valid || state.StripeCustomerID.String != "cus_user_test" {
		t.Fatalf("stripe customer: %+v", state.StripeCustomerID)
	}

	// Lookup by customer id.
	got, err := billingdb.New().GetUserBillingStateByStripeCustomer(context.Background(), pool, pgtype.Text{String: "cus_user_test", Valid: true})
	if err != nil {
		t.Fatalf("GetUserBillingStateByStripeCustomer: %v", err)
	}
	if got.UserID != uID {
		t.Errorf("lookup user_id: got %d, want %d", got.UserID, uID)
	}
}
