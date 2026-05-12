// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	stripeapi "github.com/stripe/stripe-go/v85"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker/jobs"
)

const billingFixtureHash = "$argon2id$v=19$m=16384,t=1,p=1$" +
	"AAAAAAAAAAAAAAAA$" +
	"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func TestOrgBillingSeatSyncUpdatesStateAndStripeQuantity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupOrgBillingSeatSync(t)
	memberID := createBillingUser(t, pool, "bob")
	if err := orgs.AddMember(ctx, orgs.Deps{Pool: pool, Logger: discardLogger()}, orgID, memberID, 0, "member"); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if _, err := billing.SetStripeCustomer(ctx, billing.Deps{Pool: pool}, orgID, "cus_test"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := billing.ApplySubscriptionSnapshot(ctx, billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_test",
		StripeSubscriptionItemID: "si_test",
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_test",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}

	var got stripebilling.SeatQuantityInput
	handler := jobs.OrgBillingSeatSync(jobs.OrgBillingSeatSyncDeps{
		Pool:   pool,
		Logger: discardLogger(),
		Stripe: &fakeSeatSyncStripeRemote{
			updateQuantityFn: func(_ context.Context, in stripebilling.SeatQuantityInput) error {
				got = in
				return nil
			},
		},
	})
	payload, _ := json.Marshal(jobs.OrgBillingSeatSyncPayload{OrgID: orgID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("OrgBillingSeatSync: %v", err)
	}

	state, err := billing.GetOrgBillingState(ctx, billing.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.BillableSeats != 2 || !state.SeatSnapshotAt.Valid {
		t.Fatalf("seat snapshot not reflected in state: %+v", state)
	}
	if got.OrgID != orgID || got.SubscriptionItemID != "si_test" || got.Quantity != 2 {
		t.Fatalf("unexpected stripe quantity update: %+v", got)
	}
}

func TestOrgBillingSeatSyncSkipsStripeForFreeOrg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool, orgID := setupOrgBillingSeatSync(t)
	called := false
	handler := jobs.OrgBillingSeatSync(jobs.OrgBillingSeatSyncDeps{
		Pool:   pool,
		Logger: discardLogger(),
		Stripe: &fakeSeatSyncStripeRemote{
			updateQuantityFn: func(_ context.Context, _ stripebilling.SeatQuantityInput) error {
				called = true
				return nil
			},
		},
	})
	payload, _ := json.Marshal(jobs.OrgBillingSeatSyncPayload{OrgID: orgID})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("OrgBillingSeatSync: %v", err)
	}
	if called {
		t.Fatal("expected free org seat sync to skip Stripe quantity update")
	}
	state, err := billing.GetOrgBillingState(ctx, billing.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.BillableSeats != 1 || !state.SeatSnapshotAt.Valid {
		t.Fatalf("free org seat snapshot not recorded: %+v", state)
	}
}

func setupOrgBillingSeatSync(t *testing.T) (*pgxpool.Pool, int64) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	ctx := context.Background()
	ownerID := createBillingUser(t, pool, "owner")
	org, err := orgs.Create(ctx, orgs.Deps{Pool: pool, Logger: discardLogger()}, orgs.CreateParams{
		Slug: "acme", DisplayName: "Acme Inc", CreatedByUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	return pool, org.ID
}

func createBillingUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	user, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username: username, DisplayName: username, PasswordHash: billingFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", username, err)
	}
	return user.ID
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type fakeSeatSyncStripeRemote struct {
	updateQuantityFn func(context.Context, stripebilling.SeatQuantityInput) error
}

func (f *fakeSeatSyncStripeRemote) CreateCustomer(context.Context, stripebilling.CustomerInput) (stripebilling.Customer, error) {
	return stripebilling.Customer{}, errors.New("unexpected CreateCustomer call")
}

func (f *fakeSeatSyncStripeRemote) CreateCheckoutSession(context.Context, stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error) {
	return stripebilling.CheckoutSession{}, errors.New("unexpected CreateCheckoutSession call")
}

func (f *fakeSeatSyncStripeRemote) CreatePortalSession(context.Context, stripebilling.PortalInput) (stripebilling.PortalSession, error) {
	return stripebilling.PortalSession{}, errors.New("unexpected CreatePortalSession call")
}

func (f *fakeSeatSyncStripeRemote) UpdateSubscriptionItemQuantity(ctx context.Context, in stripebilling.SeatQuantityInput) error {
	if f.updateQuantityFn == nil {
		return nil
	}
	return f.updateQuantityFn(ctx, in)
}

func (f *fakeSeatSyncStripeRemote) VerifyWebhook([]byte, string) (stripeapi.Event, error) {
	return stripeapi.Event{}, errors.New("unexpected VerifyWebhook call")
}
