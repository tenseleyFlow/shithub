// SPDX-License-Identifier: AGPL-3.0-or-later

package billing_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

const fixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func setup(t *testing.T) (*pgxpool.Pool, billing.Deps, orgsdb.Org) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	u, err := usersdb.New().CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username: "alice", DisplayName: "Alice", PasswordHash: fixtureHash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	odeps := orgs.Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	org, err := orgs.Create(ctx, odeps, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: u.ID,
	})
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return pool, billing.Deps{Pool: pool}, org
}

func TestBillingStateTransitions(t *testing.T) {
	pool, deps, org := setup(t)
	ctx := context.Background()

	state, err := billing.GetOrgBillingState(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.Plan != billing.PlanFree || state.SubscriptionStatus != billing.SubscriptionStatusNone {
		t.Fatalf("new org state: plan=%s status=%s", state.Plan, state.SubscriptionStatus)
	}

	state, err = billing.SetStripeCustomer(ctx, deps, org.ID, "cus_test")
	if err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	if !state.StripeCustomerID.Valid || state.StripeCustomerID.String != "cus_test" {
		t.Fatalf("stripe customer not set: %+v", state.StripeCustomerID)
	}

	start := time.Now().UTC().Truncate(time.Second)
	state, err = billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
		OrgID:                    org.ID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_test",
		StripeSubscriptionItemID: "si_test",
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_active",
	})
	if err != nil {
		t.Fatalf("ApplySubscriptionSnapshot active: %v", err)
	}
	assertState(t, state, billing.PlanTeam, billing.SubscriptionStatusActive)
	if state.LockedAt.Valid || state.LockReason.Valid {
		t.Fatalf("active subscription should not be locked: %+v", state)
	}
	assertOrgPlan(t, pool, org.ID, orgsdb.OrgPlanTeam)

	grace := start.Add(7 * 24 * time.Hour)
	state, err = billing.MarkPastDue(ctx, deps, org.ID, grace, "evt_past_due")
	if err != nil {
		t.Fatalf("MarkPastDue: %v", err)
	}
	assertState(t, state, billing.PlanTeam, billing.SubscriptionStatusPastDue)
	if !state.LockedAt.Valid || !state.LockReason.Valid || state.LockReason.BillingLockReason != billingdb.BillingLockReasonPastDue {
		t.Fatalf("past_due should set lock fields: %+v", state)
	}
	if !state.GraceUntil.Valid {
		t.Fatalf("past_due should set grace_until")
	}

	state, err = billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
		OrgID:                    org.ID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_test",
		StripeSubscriptionItemID: "si_test",
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_recovered",
	})
	if err != nil {
		t.Fatalf("ApplySubscriptionSnapshot recovered: %v", err)
	}
	assertState(t, state, billing.PlanTeam, billing.SubscriptionStatusActive)
	if state.LockedAt.Valid || state.LockReason.Valid || state.GraceUntil.Valid || state.PastDueAt.Valid {
		t.Fatalf("recovered subscription should clear lock/grace/past_due: %+v", state)
	}

	state, err = billing.MarkCanceled(ctx, deps, org.ID, "evt_canceled")
	if err != nil {
		t.Fatalf("MarkCanceled: %v", err)
	}
	assertState(t, state, billing.PlanFree, billing.SubscriptionStatusCanceled)
	if !state.LockedAt.Valid || !state.LockReason.Valid || state.LockReason.BillingLockReason != billingdb.BillingLockReasonCanceled {
		t.Fatalf("canceled subscription should set canceled lock: %+v", state)
	}
	assertOrgPlan(t, pool, org.ID, orgsdb.OrgPlanFree)

	state, err = billing.ClearBillingLock(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("ClearBillingLock: %v", err)
	}
	assertState(t, state, billing.PlanFree, billing.SubscriptionStatusNone)
	if state.LockedAt.Valid || state.LockReason.Valid || state.GraceUntil.Valid {
		t.Fatalf("free state should clear billing lock: %+v", state)
	}
}

func TestRecordWebhookEventIsIdempotent(t *testing.T) {
	_, deps, _ := setup(t)
	ctx := context.Background()

	event := billing.WebhookEvent{
		ProviderEventID: "evt_test",
		EventType:       "customer.subscription.updated",
		APIVersion:      "2024-06-20",
		Payload:         []byte(`{"id":"evt_test"}`),
	}
	row, created, err := billing.RecordWebhookEvent(ctx, deps, event)
	if err != nil {
		t.Fatalf("RecordWebhookEvent first: %v", err)
	}
	if !created || row.ProviderEventID != "evt_test" {
		t.Fatalf("first receipt created=%v row=%+v", created, row)
	}

	dup, created, err := billing.RecordWebhookEvent(ctx, deps, event)
	if err != nil {
		t.Fatalf("RecordWebhookEvent duplicate: %v", err)
	}
	if created {
		t.Fatalf("duplicate receipt should not be created")
	}
	if dup.ID != row.ID || dup.ProcessedAt.Valid {
		t.Fatalf("duplicate should return existing unprocessed receipt: first=%+v dup=%+v", row, dup)
	}

	if _, err := billing.MarkWebhookEventProcessed(ctx, deps, event.ProviderEventID); err != nil {
		t.Fatalf("MarkWebhookEventProcessed: %v", err)
	}
	dup, created, err = billing.RecordWebhookEvent(ctx, deps, event)
	if err != nil {
		t.Fatalf("RecordWebhookEvent after processed: %v", err)
	}
	if created {
		t.Fatalf("processed duplicate should not be created")
	}
	if dup.ID != row.ID || !dup.ProcessedAt.Valid {
		t.Fatalf("processed duplicate should return existing processed receipt: first=%+v dup=%+v", row, dup)
	}
	if _, err := billing.MarkWebhookEventFailed(ctx, deps, event.ProviderEventID, "late duplicate"); err != nil {
		t.Fatalf("MarkWebhookEventFailed: %v", err)
	}
}

func TestSyncSeatSnapshotUpdatesBillingState(t *testing.T) {
	_, deps, org := setup(t)
	ctx := context.Background()

	snap, err := billing.SyncSeatSnapshot(ctx, deps, billing.SeatSnapshot{
		OrgID:                org.ID,
		StripeSubscriptionID: "sub_test",
		ActiveMembers:        2,
		BillableSeats:        2,
	})
	if err != nil {
		t.Fatalf("SyncSeatSnapshot: %v", err)
	}
	if snap.ActiveMembers != 2 || snap.BillableSeats != 2 || snap.Source != "local" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	state, err := billing.GetOrgBillingState(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.BillableSeats != 2 || !state.SeatSnapshotAt.Valid {
		t.Fatalf("state did not record seat snapshot: %+v", state)
	}

	count, err := billing.CountBillableOrgMembers(ctx, deps, org.ID)
	if err != nil {
		t.Fatalf("CountBillableOrgMembers: %v", err)
	}
	if count != 1 {
		t.Fatalf("billable members: got %d, want 1", count)
	}
}

func TestStripeLookupsAndInvoiceSnapshot(t *testing.T) {
	_, deps, org := setup(t)
	ctx := context.Background()

	start := time.Now().UTC().Truncate(time.Second)
	if _, err := billing.SetStripeCustomer(ctx, deps, org.ID, "cus_lookup"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	if _, err := billing.ApplySubscriptionSnapshot(ctx, deps, billing.SubscriptionSnapshot{
		OrgID:                    org.ID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_lookup",
		StripeSubscriptionItemID: "si_lookup",
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_lookup",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}

	byCustomer, err := billing.GetOrgBillingStateByStripeCustomer(ctx, deps, "cus_lookup")
	if err != nil {
		t.Fatalf("GetOrgBillingStateByStripeCustomer: %v", err)
	}
	if byCustomer.OrgID != org.ID {
		t.Fatalf("customer lookup org_id: got %d, want %d", byCustomer.OrgID, org.ID)
	}
	bySubscription, err := billing.GetOrgBillingStateByStripeSubscription(ctx, deps, "sub_lookup")
	if err != nil {
		t.Fatalf("GetOrgBillingStateByStripeSubscription: %v", err)
	}
	if bySubscription.OrgID != org.ID {
		t.Fatalf("subscription lookup org_id: got %d, want %d", bySubscription.OrgID, org.ID)
	}

	invoice, err := billing.UpsertInvoice(ctx, deps, billing.InvoiceSnapshot{
		OrgID:                org.ID,
		StripeInvoiceID:      "in_lookup",
		StripeCustomerID:     "cus_lookup",
		StripeSubscriptionID: "sub_lookup",
		Status:               billing.InvoiceStatusPaid,
		Number:               "SHI-0001",
		Currency:             "USD",
		AmountDueCents:       1200,
		AmountPaidCents:      1200,
		AmountRemainingCents: 0,
		HostedInvoiceURL:     "https://invoice.stripe.test/i",
		InvoicePDFURL:        "https://invoice.stripe.test/i.pdf",
		PeriodStart:          start,
		PeriodEnd:            start.Add(30 * 24 * time.Hour),
		PaidAt:               start.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("UpsertInvoice: %v", err)
	}
	if invoice.StripeInvoiceID != "in_lookup" || invoice.Status != billing.InvoiceStatusPaid || invoice.Currency != "usd" {
		t.Fatalf("unexpected invoice: %+v", invoice)
	}
}

func assertState(t *testing.T, state billing.State, plan billing.Plan, status billing.SubscriptionStatus) {
	t.Helper()
	if state.Plan != plan || state.SubscriptionStatus != status {
		t.Fatalf("state: want plan=%s status=%s, got plan=%s status=%s", plan, status, state.Plan, state.SubscriptionStatus)
	}
}

func assertOrgPlan(t *testing.T, pool *pgxpool.Pool, orgID int64, want orgsdb.OrgPlan) {
	t.Helper()
	row, err := orgsdb.New().GetOrgByID(context.Background(), pool, orgID)
	if err != nil {
		t.Fatalf("GetOrgByID: %v", err)
	}
	if row.Plan != want {
		t.Fatalf("org plan: want %s, got %s", want, row.Plan)
	}
}
