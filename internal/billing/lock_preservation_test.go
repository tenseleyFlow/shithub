// SPDX-License-Identifier: AGPL-3.0-or-later

package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
)

// PRO08 D1 — lock-column preservation across snapshot applies.
//
// Stripe delivers `customer.subscription.updated[status=past_due]` and
// `invoice.payment_failed` in unguaranteed order. Pre-PRO08, the
// snapshot CTE unconditionally NULLed locked_at/grace_until/lock_reason,
// so a subscription.updated arriving AFTER invoice.payment_failed
// wiped the grace lock — the user would see the "Pro features are
// read-only" banner immediately instead of staying in grace.
//
// These tests pin the fix: the snapshot path preserves existing lock
// columns when status=past_due, and clears them ONLY when transitioning
// from past_due/unpaid/incomplete → active/trialing (the recovery path
// that MarkPaymentSucceeded normally drives, but subscription.updated
// status flips also need to handle it).

func TestApplySubscriptionSnapshotPreservesGraceLockOnPastDue(t *testing.T) {
	_, deps, org := setup(t)
	ctx := context.Background()

	// 1. Establish past_due via the invoice path — MarkPastDue sets
	//    locked_at + grace_until.
	graceUntil := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	if _, err := billing.MarkPastDue(ctx, deps, org.ID, graceUntil, "evt_past_due"); err != nil {
		t.Fatalf("MarkPastDue: %v", err)
	}
	before, err := billing.GetOrgBillingState(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if !before.LockedAt.Valid || !before.GraceUntil.Valid {
		t.Fatalf("expected locked + grace set after MarkPastDue, got %+v", before)
	}

	// 2. customer.subscription.updated[status=past_due] arrives next.
	//    Pre-fix: this wiped the lock. Post-fix: COALESCE preserves it.
	_, err = billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
		OrgID:                org.ID,
		Plan:                 billing.PlanTeam,
		Status:               billing.SubscriptionStatusPastDue,
		StripeSubscriptionID: "sub_past_due",
		LastWebhookEventID:   "evt_sub_past_due",
	})
	if err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	after, err := billing.GetOrgBillingState(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !after.LockedAt.Valid {
		t.Fatalf("locked_at lost on subscription.updated[past_due]: %+v", after)
	}
	if !after.GraceUntil.Valid || !after.GraceUntil.Time.Equal(graceUntil) {
		t.Fatalf("grace_until lost on subscription.updated[past_due]: got %+v want %v", after.GraceUntil, graceUntil)
	}
	if !after.LockReason.Valid || after.LockReason.BillingLockReason != billingdb.BillingLockReasonPastDue {
		t.Fatalf("lock_reason lost: %+v", after.LockReason)
	}
}

// TestApplySubscriptionSnapshotClearsLocksOnPastDueRecovery covers the
// other half: subscription.updated[status=active] arriving after a
// past_due episode clears the lock columns even without an explicit
// invoice.payment_succeeded.
func TestApplySubscriptionSnapshotClearsLocksOnPastDueRecovery(t *testing.T) {
	_, deps, org := setup(t)
	ctx := context.Background()

	graceUntil := time.Now().UTC().Add(72 * time.Hour)
	if _, err := billing.MarkPastDue(ctx, deps, org.ID, graceUntil, "evt_past_due"); err != nil {
		t.Fatalf("MarkPastDue: %v", err)
	}
	// recovery via subscription.updated[active]:
	if _, err := billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
		OrgID:                org.ID,
		Plan:                 billing.PlanTeam,
		Status:               billing.SubscriptionStatusActive,
		StripeSubscriptionID: "sub_recovered",
		LastWebhookEventID:   "evt_sub_active",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot active: %v", err)
	}
	state, err := billing.GetOrgBillingState(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if state.LockedAt.Valid {
		t.Errorf("locked_at should be NULL after recovery, got %+v", state.LockedAt)
	}
	if state.GraceUntil.Valid {
		t.Errorf("grace_until should be NULL after recovery, got %+v", state.GraceUntil)
	}
	if state.LockReason.Valid {
		t.Errorf("lock_reason should be NULL after recovery, got %+v", state.LockReason)
	}
}

// TestApplyUserSubscriptionSnapshotPreservesGraceLockOnPastDue mirrors
// the org-side test for the user-tier (Pro). Same fix shape, same
// risk: subscription.updated for a Pro user in grace must not wipe
// the grace fields.
func TestApplyUserSubscriptionSnapshotPreservesGraceLockOnPastDue(t *testing.T) {
	_, deps, org := setup(t)
	ctx := context.Background()
	_ = org

	// Create a fresh user — setup gives us an owner via insertSetupUser.
	userID := insertSetupUser(t, deps, "prolocked")
	q := billingdb.New()
	// Seed Pro+active first (the trigger seeds 'free,none' by default).
	if _, err := q.ApplyUserSubscriptionSnapshot(ctx, deps.Pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_user_lock", Valid: true},
		LastWebhookEventID:   "evt_seed_pro",
	}); err != nil {
		t.Fatalf("seed Pro: %v", err)
	}
	graceUntil := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Second)
	if _, err := q.MarkUserPastDue(ctx, deps.Pool, billingdb.MarkUserPastDueParams{
		UserID:             userID,
		GraceUntil:         pgtype.Timestamptz{Time: graceUntil, Valid: true},
		LastWebhookEventID: "evt_user_past_due",
	}); err != nil {
		t.Fatalf("MarkUserPastDue: %v", err)
	}
	// Now apply subscription.updated[past_due] via the snapshot path.
	if _, err := q.ApplyUserSubscriptionSnapshot(ctx, deps.Pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusPastDue,
		StripeSubscriptionID: pgtype.Text{String: "sub_user_lock", Valid: true},
		LastWebhookEventID:   "evt_user_sub_past_due",
	}); err != nil {
		t.Fatalf("apply past_due via snapshot: %v", err)
	}
	state, err := q.GetUserBillingState(ctx, deps.Pool, userID)
	if err != nil {
		t.Fatalf("get user state: %v", err)
	}
	if !state.LockedAt.Valid {
		t.Errorf("user locked_at wiped by snapshot path: %+v", state)
	}
	if !state.GraceUntil.Valid || !state.GraceUntil.Time.Equal(graceUntil) {
		t.Errorf("user grace_until wiped/changed: got %+v want %v", state.GraceUntil, graceUntil)
	}
}

func insertSetupUser(t *testing.T, deps billing.Deps, username string) int64 {
	t.Helper()
	var id int64
	if err := deps.Pool.QueryRow(context.Background(),
		`INSERT INTO users (username, password_hash) VALUES ($1, $2) RETURNING id`,
		username,
		"$argon2id$v=19$m=16384,t=1,p=1$AAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}
