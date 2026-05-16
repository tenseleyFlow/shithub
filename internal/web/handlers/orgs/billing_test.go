// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	stripeapi "github.com/stripe/stripe-go/v85"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestOrgBillingCheckoutRedirectsToStripeAndCreatesCustomer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	fake := &fakeStripeRemote{
		createCustomerFn: func(_ context.Context, in stripebilling.CustomerInput) (stripebilling.Customer, error) {
			if in.OrgID != orgID || in.OrgSlug != "acme" {
				t.Fatalf("unexpected customer input: %+v", in)
			}
			return stripebilling.Customer{ID: "cus_test_checkout"}, nil
		},
		createCheckoutFn: func(_ context.Context, in stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error) {
			if in.CustomerID != "cus_test_checkout" {
				t.Fatalf("checkout customer = %q", in.CustomerID)
			}
			if in.SeatCount != 1 {
				t.Fatalf("checkout seats = %d, want 1", in.SeatCount)
			}
			if !strings.Contains(in.SuccessURL, "/organizations/acme/billing/success") {
				t.Fatalf("success url = %q", in.SuccessURL)
			}
			if !strings.Contains(in.CancelURL, "/organizations/acme/billing/cancel") {
				t.Fatalf("cancel url = %q", in.CancelURL)
			}
			return stripebilling.CheckoutSession{ID: "cs_test", URL: "https://checkout.stripe.test/session"}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/checkout", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("checkout status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "https://checkout.stripe.test/session" {
		t.Fatalf("checkout redirect=%q", got)
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if !state.StripeCustomerID.Valid || state.StripeCustomerID.String != "cus_test_checkout" {
		t.Fatalf("expected stripe customer saved, got %+v", state.StripeCustomerID)
	}
}

func TestOrgBillingPortalRedirectsToStripe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: pool}, orgID, "cus_test_portal"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	fake := &fakeStripeRemote{
		createPortalFn: func(_ context.Context, in stripebilling.PortalInput) (stripebilling.PortalSession, error) {
			if in.CustomerID != "cus_test_portal" {
				t.Fatalf("portal customer = %q", in.CustomerID)
			}
			if !strings.Contains(in.ReturnURL, "/organizations/acme/settings/billing") {
				t.Fatalf("portal return url = %q", in.ReturnURL)
			}
			return stripebilling.PortalSession{ID: "bps_test", URL: "https://billing.stripe.test/session"}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/portal", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("portal status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "https://billing.stripe.test/session" {
		t.Fatalf("portal redirect=%q", got)
	}
}

func TestOrgBillingResultPagesRenderPostCheckoutState(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	insertOrgAvatarOrg(t, pool, ownerID, "acme")
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/organizations/acme/billing/success", want: "RESULT=success;HEADING=Checkout complete"},
		{path: "/organizations/acme/billing/cancel", want: "RESULT=canceled;HEADING=Checkout canceled"},
	} {
		resp := httptest.NewRecorder()
		req := newOrgFormRequest(http.MethodGet, tc.path, nil)
		mux.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.path, resp.Code, resp.Body.String())
		}
		if !strings.Contains(resp.Body.String(), tc.want) {
			t.Fatalf("%s missing %q in body %s", tc.path, tc.want, resp.Body.String())
		}
	}
}

func TestOrgBillingSettingsRequiresOwner(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	insertOrgAvatarOrg(t, pool, ownerID, "acme")
	memberID := insertOrgAvatarUser(t, pool, "member")
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: memberID, Username: "member"}, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("settings status=%d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing/licensing", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("licensing status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestOrgBillingSettingsRendersSeatBreakdownAndHidesStripeIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: pool}, orgID, "cus_owner_secret"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_owner_secret",
		StripeSubscriptionItemID: "si_owner_secret",
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_owner_secret",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	if _, err := orgbilling.SetTeamLicensedSeats(ctx, orgbilling.Deps{Pool: pool}, orgID, 3, "test"); err != nil {
		t.Fatalf("SetTeamLicensedSeats: %v", err)
	}
	insertBillingPendingInvitation(t, pool, orgID, "pending@example.com", []byte{1, 2, 3})
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "SEATS=1/3/2/1;") {
		t.Fatalf("settings did not render seat breakdown: %s", body)
	}
	if strings.Contains(body, "cus_owner_secret") {
		t.Fatalf("owner billing page leaked Stripe customer id: %s", body)
	}
}

func TestOrgBillingLicensingRendersSeatConsumersAndActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	memberID := insertOrgAvatarUser(t, pool, "member")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	start := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_licensing",
		StripeSubscriptionItemID: "si_licensing",
		LicensedSeats:            4,
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	insertBillingPendingInvitation(t, pool, orgID, "pending@example.com", []byte{5, 6, 7})
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing/licensing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("licensing status=%d body=%s", resp.Code, body)
	}
	for _, want := range []string{
		"LICENSE=2/4/2/1;",
		"CONSUMER=owner:Owner;",
		"CONSUMER=member:Member;",
		"PENDING=pending@example.com;",
		"ADD=true;REMOVE=true;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("licensing missing %q in body %s", want, body)
		}
	}
}

func TestOrgBillingAddSeatsUpdatesStripeQuantity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	start := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: pool}, orgID, "cus_add"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_add",
		StripeSubscriptionItemID: "si_add",
		LicensedSeats:            2,
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	var previewed int64
	var applied stripebilling.TeamSeatChangeInput
	fake := &fakeStripeRemote{
		previewSeatChangeFn: func(_ context.Context, in stripebilling.TeamSeatPreviewInput) (stripebilling.TeamSeatPreview, error) {
			if in.CustomerID != "cus_add" || in.SubscriptionID != "sub_add" || in.SubscriptionItemID != "si_add" {
				t.Fatalf("preview identifiers = %+v", in)
			}
			previewed = in.NewQuantity
			return stripebilling.TeamSeatPreview{Currency: "usd", CurrentPeriodAmount: 533, AmountDue: 533, ProrationDate: in.ProrationDate}, nil
		},
		applySeatChangeFn: func(_ context.Context, in stripebilling.TeamSeatChangeInput) error {
			if in.SubscriptionItemID != "si_add" {
				t.Fatalf("subscription item = %q", in.SubscriptionItemID)
			}
			applied = in
			return nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/billing/seats/add", url.Values{
		"additional_seats": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("add seats status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/organizations/acme/settings/billing/licensing?notice=seats-added" {
		t.Fatalf("add redirect=%q", got)
	}
	if previewed != 4 {
		t.Fatalf("preview quantity = %d, want 4", previewed)
	}
	if applied.NewQuantity != 4 {
		t.Fatalf("stripe quantity = %d, want 4", applied.NewQuantity)
	}
	if applied.IdempotencyKey == "" {
		t.Fatal("apply should include an idempotency key")
	}
	if applied.ProrationDate == 0 {
		t.Fatal("apply should use the preview proration date")
	}
	state, err := orgbilling.GetTeamLicenseState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetTeamLicenseState: %v", err)
	}
	if state.LicensedSeats != 4 || state.UsedSeats != 1 || state.AvailableSeats != 3 {
		t.Fatalf("unexpected license state: %+v", state)
	}
}

func TestOrgBillingAddSeatsStripeFailureLeavesLocalSeatsUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: pool}, orgID, "cus_add_failed"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	start := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_add_failed",
		StripeSubscriptionItemID: "si_add_failed",
		LicensedSeats:            2,
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	fake := &fakeStripeRemote{
		previewSeatChangeFn: func(_ context.Context, in stripebilling.TeamSeatPreviewInput) (stripebilling.TeamSeatPreview, error) {
			return stripebilling.TeamSeatPreview{Currency: "usd", CurrentPeriodAmount: 533, ProrationDate: in.ProrationDate}, nil
		},
		applySeatChangeFn: func(context.Context, stripebilling.TeamSeatChangeInput) error {
			return errors.New("stripe rejected quantity update")
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/billing/seats/add", url.Values{
		"additional_seats": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("add seats failure status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ERROR=Could not add seats right now.") {
		t.Fatalf("failure did not render add-seat error: %s", resp.Body.String())
	}
	state, err := orgbilling.GetTeamLicenseState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetTeamLicenseState: %v", err)
	}
	if state.LicensedSeats != 2 || state.AvailableSeats != 1 {
		t.Fatalf("local seats changed after Stripe failure: %+v", state)
	}
}

func TestOrgBillingRemoveSeatsRejectsBelowUsedAndHidesWhenUnavailable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	memberID := insertOrgAvatarUser(t, pool, "member")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	start := time.Now().UTC().Add(-24 * time.Hour)
	if _, err := orgbilling.SetStripeCustomer(ctx, orgbilling.Deps{Pool: pool}, orgID, "cus_remove"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_remove",
		StripeSubscriptionItemID: "si_remove",
		LicensedSeats:            4,
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	var previewCalls int
	var applyCalls int
	fake := &fakeStripeRemote{
		previewSeatChangeFn: func(_ context.Context, in stripebilling.TeamSeatPreviewInput) (stripebilling.TeamSeatPreview, error) {
			previewCalls++
			if in.NewQuantity != 2 {
				t.Fatalf("preview quantity = %d, want 2", in.NewQuantity)
			}
			return stripebilling.TeamSeatPreview{Currency: "usd", CurrentPeriodAmount: -350, ProrationDate: in.ProrationDate}, nil
		},
		applySeatChangeFn: func(_ context.Context, in stripebilling.TeamSeatChangeInput) error {
			applyCalls++
			if in.NewQuantity != 2 {
				t.Fatalf("stripe quantity = %d, want 2", in.NewQuantity)
			}
			return nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/billing/seats/remove", url.Values{
		"remove_seats": {"3"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("invalid remove status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ERROR=Licensed seats cannot be reduced below the number of people consuming seats.") {
		t.Fatalf("invalid remove did not explain used-seat floor: %s", resp.Body.String())
	}
	if previewCalls != 0 || applyCalls != 0 {
		t.Fatalf("stripe update called for invalid removal")
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/billing/seats/remove", url.Values{
		"remove_seats": {"2"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("valid remove status=%d body=%s", resp.Code, resp.Body.String())
	}
	if previewCalls != 1 || applyCalls != 1 {
		t.Fatalf("stripe calls preview=%d apply=%d, want 1/1", previewCalls, applyCalls)
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing/seats/remove", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("remove with no available seats status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestOrgBillingAddSeatsUnavailableForFreeOrg(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	insertOrgAvatarOrg(t, pool, ownerID, "acme")
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing/seats/add", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("free add seats status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestOrgInviteFullTeamLinksToAddSeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	insertOrgAvatarUser(t, pool, "member")
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_full",
		StripeSubscriptionItemID: "si_full",
		LicensedSeats:            1,
		CurrentPeriodStart:       time.Now().UTC().Add(-time.Hour),
		CurrentPeriodEnd:         time.Now().UTC().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/acme/people/invite", url.Values{
		"target": {"member"},
		"role":   {"member"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("invite status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/acme/people?notice=team-seat-limit" {
		t.Fatalf("invite redirect=%q", got)
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodGet, "/acme/people?notice=team-seat-limit", nil)
	mux.ServeHTTP(resp, req)
	if !strings.Contains(resp.Body.String(), "ACTION=Add seats|/organizations/acme/settings/billing/seats/add;") {
		t.Fatalf("people notice did not link add seats: %s", resp.Body.String())
	}
}

func TestOrgBillingSettingsRendersUsageThresholds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	start, end := orgbilling.MonthlyUsagePeriod(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	if _, err := orgbilling.UpsertOrgUsageCounters(ctx, orgbilling.Deps{Pool: pool}, orgbilling.UsageCounterSnapshot{
		OrgID:                orgID,
		RepoStorageBytes:     490 * 1024 * 1024,
		ObjectStorageBytes:   0,
		ActionsLogBytes:      0,
		ActionsArtifactBytes: 0,
		ActionsMinutesUsed:   1600,
		ActionsPeriodStart:   start,
		ActionsPeriodEnd:     end,
		CalculatedAt:         start.Add(12 * time.Hour),
	}); err != nil {
		t.Fatalf("UpsertOrgUsageCounters: %v", err)
	}
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "USAGE=storage:490 MiB/500 MiB/98%/is-danger;") {
		t.Fatalf("settings did not render storage usage threshold: %s", body)
	}
	if !strings.Contains(body, "USAGE=actions-minutes:1600 minutes/2000 minutes/80%/is-warning;") {
		t.Fatalf("settings did not render actions usage threshold: %s", body)
	}
	if !strings.Contains(body, "USAGE_ALERT=This organization has used at least 95% of its storage quota.") {
		t.Fatalf("settings did not render quota warning: %s", body)
	}
}

func TestOrgBillingSettingsAppliesQuotaOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	start, end := orgbilling.MonthlyUsagePeriod(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	deps := orgbilling.Deps{Pool: pool}
	if _, err := orgbilling.UpsertOrgUsageCounters(ctx, deps, orgbilling.UsageCounterSnapshot{
		OrgID:                orgID,
		RepoStorageBytes:     6 * 1024 * 1024 * 1024,
		ObjectStorageBytes:   0,
		ActionsLogBytes:      0,
		ActionsArtifactBytes: 0,
		ActionsMinutesUsed:   2100,
		ActionsPeriodStart:   start,
		ActionsPeriodEnd:     end,
		CalculatedAt:         start.Add(12 * time.Hour),
	}); err != nil {
		t.Fatalf("UpsertOrgUsageCounters: %v", err)
	}
	if _, err := orgbilling.UpsertOrgQuotaOverride(ctx, deps, orgbilling.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            orgbilling.QuotaKindStorageBytes,
		LimitValue:      10 * 1024 * 1024 * 1024,
		Note:            "temporary migration",
		CreatedByUserID: ownerID,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride storage: %v", err)
	}
	if _, err := orgbilling.UpsertOrgQuotaOverride(ctx, deps, orgbilling.QuotaOverrideInput{
		OrgID:           orgID,
		Kind:            orgbilling.QuotaKindActionsMinutes,
		Unlimited:       true,
		CreatedByUserID: ownerID,
	}); err != nil {
		t.Fatalf("UpsertOrgQuotaOverride actions: %v", err)
	}
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner", IsSiteAdmin: true}, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "USAGE=storage:6 GiB/10 GiB override/60%/is-ok;") {
		t.Fatalf("settings did not apply storage override: %s", body)
	}
	if !strings.Contains(body, "USAGE=actions-minutes:2100 minutes/Unlimited override/Unlimited/is-ok;") {
		t.Fatalf("settings did not apply actions override: %s", body)
	}
	if !strings.Contains(body, "OVERRIDE=Storage:10 GiB;") || !strings.Contains(body, "OVERRIDE=Actions minutes:Unlimited;") {
		t.Fatalf("settings did not render site-admin quota overrides: %s", body)
	}
}

func TestOrgBillingSiteAdminCanManageQuotaOverrides(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	adminID := insertOrgAvatarUser(t, pool, "admin")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: adminID, Username: "admin", IsSiteAdmin: true}, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/quota-overrides", url.Values{
		"kind":        {"storage_bytes"},
		"limit_value": {"1073741824"},
		"note":        {"support migration"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("save status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/organizations/acme/settings/billing?notice=quota-override-saved" {
		t.Fatalf("save redirect=%q", got)
	}
	override, err := orgbilling.GetOrgQuotaOverride(ctx, orgbilling.Deps{Pool: pool}, orgID, orgbilling.QuotaKindStorageBytes)
	if err != nil {
		t.Fatalf("GetOrgQuotaOverride: %v", err)
	}
	if override.Unlimited || !override.LimitValue.Valid || override.LimitValue.Int64 != 1073741824 || override.Note != "support migration" {
		t.Fatalf("unexpected override: %+v", override)
	}
	if !override.CreatedByUserID.Valid || override.CreatedByUserID.Int64 != adminID {
		t.Fatalf("created_by_user_id=%+v, want %d", override.CreatedByUserID, adminID)
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/quota-overrides", url.Values{
		"kind":      {"actions_minutes"},
		"unlimited": {"1"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("unlimited save status=%d body=%s", resp.Code, resp.Body.String())
	}
	actionsOverride, err := orgbilling.GetOrgQuotaOverride(ctx, orgbilling.Deps{Pool: pool}, orgID, orgbilling.QuotaKindActionsMinutes)
	if err != nil {
		t.Fatalf("GetOrgQuotaOverride actions: %v", err)
	}
	if !actionsOverride.Unlimited || actionsOverride.LimitValue.Valid {
		t.Fatalf("unexpected unlimited override: %+v", actionsOverride)
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/quota-overrides/delete", url.Values{
		"kind": {"storage_bytes"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("delete status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/organizations/acme/settings/billing?notice=quota-override-cleared" {
		t.Fatalf("delete redirect=%q", got)
	}
	overrides, err := orgbilling.ListOrgQuotaOverrides(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("ListOrgQuotaOverrides: %v", err)
	}
	if len(overrides) != 1 || overrides[0].Kind != orgbilling.QuotaKindActionsMinutes {
		t.Fatalf("unexpected remaining overrides: %+v", overrides)
	}
}

func TestOrgBillingQuotaOverridesRequireSiteAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/quota-overrides", url.Values{
		"kind":        {"storage_bytes"},
		"limit_value": {"1"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("save status=%d body=%s", resp.Code, resp.Body.String())
	}
	overrides, err := orgbilling.ListOrgQuotaOverrides(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("ListOrgQuotaOverrides: %v", err)
	}
	if len(overrides) != 0 {
		t.Fatalf("non-admin created overrides: %+v", overrides)
	}
}

func TestOrgBillingSettingsSiteAdminDebugShowsProviderState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	deps := orgbilling.Deps{Pool: pool}
	if _, err := orgbilling.SetStripeCustomer(ctx, deps, orgID, "cus_debug"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, deps, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_debug",
		StripeSubscriptionItemID: "si_debug",
		CurrentPeriodStart:       time.Now().UTC().Add(-time.Hour),
		CurrentPeriodEnd:         time.Now().UTC().Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_debug",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	if _, _, err := orgbilling.RecordWebhookEvent(ctx, deps, orgbilling.WebhookEvent{
		ProviderEventID: "evt_debug",
		EventType:       "customer.subscription.updated",
		APIVersion:      "2024-06-20",
		Payload:         []byte(`{"id":"evt_debug"}`),
	}); err != nil {
		t.Fatalf("RecordWebhookEvent: %v", err)
	}
	if _, err := orgbilling.MarkWebhookEventProcessed(ctx, deps, "evt_debug"); err != nil {
		t.Fatalf("MarkWebhookEventProcessed: %v", err)
	}
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner", IsSiteAdmin: true}, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "DEBUG=cus_debug|sub_debug|si_debug|evt_debug|processed;") {
		t.Fatalf("settings did not render site-admin debug state: %s", body)
	}
}

func TestOrgBillingSettingsSiteAdminDebugDetectsSeatDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_drift",
		StripeSubscriptionItemID: "si_drift",
		LicensedSeats:            3,
		CurrentPeriodStart:       time.Now().UTC().Add(-time.Hour),
		CurrentPeriodEnd:         time.Now().UTC().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	fake := &fakeStripeRemote{
		fetchSeatQuantityFn: func(_ context.Context, subscriptionItemID string) (int64, error) {
			if subscriptionItemID != "si_drift" {
				t.Fatalf("subscription item = %q", subscriptionItemID)
			}
			return 5, nil
		},
	}
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner", IsSiteAdmin: true}, fake)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "DRIFT=Drift detected|3|5|true;") {
		t.Fatalf("settings did not render seat drift: %s", body)
	}
}

func TestOrgBillingSiteAdminRepairsSeatDriftFromStripe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	adminID := insertOrgAvatarUser(t, pool, "admin")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_repair",
		StripeSubscriptionItemID: "si_repair",
		LicensedSeats:            3,
		CurrentPeriodStart:       time.Now().UTC().Add(-time.Hour),
		CurrentPeriodEnd:         time.Now().UTC().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: adminID, Username: "admin", IsSiteAdmin: true}, &fakeStripeRemote{
		fetchSeatQuantityFn: func(_ context.Context, subscriptionItemID string) (int64, error) {
			if subscriptionItemID != "si_repair" {
				t.Fatalf("subscription item = %q", subscriptionItemID)
			}
			return 5, nil
		},
	})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/seat-drift/repair", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("repair status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/organizations/acme/settings/billing?notice=seat-drift-repaired" {
		t.Fatalf("repair redirect=%q", got)
	}
	state, err := orgbilling.GetTeamLicenseState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetTeamLicenseState: %v", err)
	}
	if state.LicensedSeats != 5 {
		t.Fatalf("licensed seats=%d, want 5", state.LicensedSeats)
	}
	rows, err := usersdb.New().ListAuditLogForTarget(ctx, pool, usersdb.ListAuditLogForTargetParams{
		TargetType: "org",
		TargetID:   pgtype.Int8{Int64: orgID, Valid: true},
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListAuditLogForTarget: %v", err)
	}
	if len(rows) != 1 || rows[0].Action != "admin_org_billing_seats_repaired" {
		t.Fatalf("unexpected audit rows: %+v", rows)
	}
	if !rows[0].ActorID.Valid || rows[0].ActorID.Int64 != adminID {
		t.Fatalf("audit actor=%+v, want %d", rows[0].ActorID, adminID)
	}
	var meta map[string]any
	if err := json.Unmarshal(rows[0].Meta, &meta); err != nil {
		t.Fatalf("audit meta unmarshal: %v", err)
	}
	if meta["stripe_subscription_item_id"] != "si_repair" || meta["source"] != "stripe_repair" {
		t.Fatalf("unexpected audit meta: %v", meta)
	}
}

func TestOrgBillingSeatDriftRepairBlocksBelowUsedSeats(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	adminID := insertOrgAvatarUser(t, pool, "admin")
	memberID := insertOrgAvatarUser(t, pool, "member")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := pool.Exec(ctx, `INSERT INTO org_members (org_id, user_id, role) VALUES ($1, $2, 'member')`, orgID, memberID); err != nil {
		t.Fatalf("insert member: %v", err)
	}
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_low",
		StripeSubscriptionItemID: "si_low",
		LicensedSeats:            2,
		CurrentPeriodStart:       time.Now().UTC().Add(-time.Hour),
		CurrentPeriodEnd:         time.Now().UTC().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	mux := newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: adminID, Username: "admin", IsSiteAdmin: true}, &fakeStripeRemote{
		fetchSeatQuantityFn: func(context.Context, string) (int64, error) { return 1, nil },
	})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/seat-drift/repair", url.Values{})
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("repair status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "ERROR=Stripe currently reports fewer seats than this organization is using.") {
		t.Fatalf("repair did not explain below-used block: %s", body)
	}
	state, err := orgbilling.GetTeamLicenseState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetTeamLicenseState: %v", err)
	}
	if state.LicensedSeats != 2 {
		t.Fatalf("licensed seats changed to %d, want 2", state.LicensedSeats)
	}
	rows, err := usersdb.New().ListAuditLogForTarget(ctx, pool, usersdb.ListAuditLogForTargetParams{
		TargetType: "org",
		TargetID:   pgtype.Int8{Int64: orgID, Valid: true},
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListAuditLogForTarget: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("blocked repair wrote audit rows: %+v", rows)
	}
}

func TestOrgBillingSeatDriftRepairRequiresSiteAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_owner",
		StripeSubscriptionItemID: "si_owner",
		LicensedSeats:            3,
		CurrentPeriodStart:       time.Now().UTC().Add(-time.Hour),
		CurrentPeriodEnd:         time.Now().UTC().Add(30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{
		fetchSeatQuantityFn: func(context.Context, string) (int64, error) { return 5, nil },
	})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/billing/seat-drift/repair", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("repair status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetTeamLicenseState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetTeamLicenseState: %v", err)
	}
	if state.LicensedSeats != 3 {
		t.Fatalf("licensed seats changed to %d, want 3", state.LicensedSeats)
	}
}

func TestOrgBillingSettingsShowsPastDueAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.MarkPastDue(ctx, orgbilling.Deps{Pool: pool}, orgID, time.Now().UTC().Add(24*time.Hour), "evt_failed"); err != nil {
		t.Fatalf("MarkPastDue: %v", err)
	}
	mux := newOrgBillingMux(t, pool, ownerID, &fakeStripeRemote{})

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodGet, "/organizations/acme/settings/billing", nil)
	mux.ServeHTTP(resp, req)
	body := resp.Body.String()
	if resp.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", resp.Code, body)
	}
	if !strings.Contains(body, "ALERT=Payment failed.") {
		t.Fatalf("settings did not render past-due alert: %s", body)
	}
}

func TestOrgBillingWebhookProcessesSubscriptionAndStaysIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	raw, err := json.Marshal(map[string]any{
		"id":                   "sub_test",
		"customer":             "cus_test_webhook",
		"status":               "active",
		"cancel_at_period_end": false,
		"trial_end":            int64(0),
		"canceled_at":          int64(0),
		"metadata":             map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_test_webhook",
			"quantity":             int64(5),
			"current_period_start": time.Now().UTC().Add(-time.Hour).Unix(),
			"current_period_end":   time.Now().UTC().Add(30 * 24 * time.Hour).Unix(),
		}}},
	})
	if err != nil {
		t.Fatalf("marshal subscription raw: %v", err)
	}
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:         "evt_sub_active",
				Type:       stripeapi.EventType("customer.subscription.updated"),
				APIVersion: "2024-06-20",
				Data:       &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	req := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(`{"id":"evt_sub_active"}`))
	req.Header.Set("Stripe-Signature", "sig_test")
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("first webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if state.Plan != orgbilling.PlanTeam || state.SubscriptionStatus != orgbilling.SubscriptionStatusActive {
		t.Fatalf("unexpected billing state: %+v", state)
	}
	if !state.StripeCustomerID.Valid || state.StripeCustomerID.String != "cus_test_webhook" {
		t.Fatalf("expected customer id saved, got %+v", state.StripeCustomerID)
	}
	if !state.StripeSubscriptionID.Valid || state.StripeSubscriptionID.String != "sub_test" {
		t.Fatalf("expected subscription id saved, got %+v", state.StripeSubscriptionID)
	}
	if state.LicensedSeats != 5 || state.UsedSeats != 1 {
		t.Fatalf("expected subscription quantity to persist as licensed seats, got licensed=%d used=%d", state.LicensedSeats, state.UsedSeats)
	}
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_sub_active")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt: %v", err)
	}
	if !receipt.ProcessedAt.Valid || receipt.ProcessingAttempts != 1 {
		t.Fatalf("unexpected receipt after first processing: %+v", receipt)
	}
	// PRO08 A2: subject must be recorded on the receipt after resolve.
	if !receipt.SubjectKind.Valid || receipt.SubjectKind.BillingSubjectKind != billingdb.BillingSubjectKindOrg {
		t.Fatalf("receipt subject_kind: got %+v, want org", receipt.SubjectKind)
	}
	if !receipt.SubjectID.Valid || receipt.SubjectID.Int64 != orgID {
		t.Fatalf("receipt subject_id: got %+v, want %d", receipt.SubjectID, orgID)
	}

	req = httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(`{"id":"evt_sub_active"}`))
	req.Header.Set("Stripe-Signature", "sig_test")
	resp = httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("duplicate webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	receipt, err = billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_sub_active")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt duplicate: %v", err)
	}
	if receipt.ProcessingAttempts != 1 {
		t.Fatalf("duplicate webhook should not reprocess receipt: %+v", receipt)
	}
}

func TestOrgBillingWebhookCheckoutCompletedStoresCustomerOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	raw := mustJSONRaw(t, map[string]any{
		"id":                  "cs_test_completed",
		"object":              "checkout.session",
		"customer":            "cus_test_checkout_completed",
		"client_reference_id": strconv.FormatInt(orgID, 10),
	})
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return stripeapi.Event{
				ID:         "evt_checkout_completed",
				Type:       stripeapi.EventType("checkout.session.completed"),
				APIVersion: "2024-06-20",
				Data:       &stripeapi.EventData{Raw: raw},
			}, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	resp := postBillingWebhook(t, mux, "evt_checkout_completed")
	if resp.Code != http.StatusOK {
		t.Fatalf("checkout webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, orgbilling.Deps{Pool: pool}, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState: %v", err)
	}
	if !state.StripeCustomerID.Valid || state.StripeCustomerID.String != "cus_test_checkout_completed" {
		t.Fatalf("expected checkout customer saved, got %+v", state.StripeCustomerID)
	}
	if state.Plan != orgbilling.PlanFree || state.SubscriptionStatus != orgbilling.SubscriptionStatusNone {
		t.Fatalf("checkout completion must not activate paid state by itself: %+v", state)
	}
}

func TestOrgBillingWebhookHandlesInvoiceFailureRecoveryAndCancellation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	deps := orgbilling.Deps{Pool: pool}
	if _, err := orgbilling.SetStripeCustomer(ctx, deps, orgID, "cus_test_lifecycle"); err != nil {
		t.Fatalf("SetStripeCustomer: %v", err)
	}
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, deps, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_test_lifecycle",
		StripeSubscriptionItemID: "si_test_lifecycle",
		CurrentPeriodStart:       start,
		CurrentPeriodEnd:         start.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_seed_active",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}

	var current stripeapi.Event
	fake := &fakeStripeRemote{
		verifyWebhookFn: func(_ []byte, _ string) (stripeapi.Event, error) {
			return current, nil
		},
	}
	mux := newOrgBillingMux(t, pool, ownerID, fake)

	current = stripeTestEvent(t, "evt_invoice_failed", "invoice.payment_failed", map[string]any{
		"id":                   "in_test_lifecycle",
		"object":               "invoice",
		"customer":             "cus_test_lifecycle",
		"status":               "open",
		"number":               "INV-FAILED",
		"currency":             "usd",
		"amount_due":           int64(400),
		"amount_paid":          int64(0),
		"amount_remaining":     int64(400),
		"hosted_invoice_url":   "https://pay.stripe.test/invoice",
		"invoice_pdf":          "https://pay.stripe.test/invoice.pdf",
		"period_start":         start.Unix(),
		"period_end":           start.Add(30 * 24 * time.Hour).Unix(),
		"due_date":             start.Add(3 * 24 * time.Hour).Unix(),
		"status_transitions":   map[string]any{},
		"subscription_details": map[string]any{},
	})
	resp := postBillingWebhook(t, mux, "evt_invoice_failed")
	if resp.Code != http.StatusOK {
		t.Fatalf("payment_failed webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err := orgbilling.GetOrgBillingState(ctx, deps, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState after failed payment: %v", err)
	}
	if state.Plan != orgbilling.PlanTeam || state.SubscriptionStatus != orgbilling.SubscriptionStatusPastDue {
		t.Fatalf("payment_failed should keep Team plan and mark past_due: %+v", state)
	}
	if !state.LockedAt.Valid || !state.LockReason.Valid || state.LockReason.BillingLockReason != billingdb.BillingLockReasonPastDue {
		t.Fatalf("payment_failed should set past_due lock fields: %+v", state)
	}
	if !state.GraceUntil.Valid {
		t.Fatalf("payment_failed should set grace_until: %+v", state)
	}

	current = stripeTestEvent(t, "evt_invoice_paid", "invoice.payment_succeeded", map[string]any{
		"id":                   "in_test_lifecycle",
		"object":               "invoice",
		"customer":             "cus_test_lifecycle",
		"status":               "paid",
		"number":               "INV-FAILED",
		"currency":             "usd",
		"amount_due":           int64(400),
		"amount_paid":          int64(400),
		"amount_remaining":     int64(0),
		"period_start":         start.Unix(),
		"period_end":           start.Add(30 * 24 * time.Hour).Unix(),
		"status_transitions":   map[string]any{"paid_at": start.Add(time.Hour).Unix()},
		"subscription_details": map[string]any{},
	})
	resp = postBillingWebhook(t, mux, "evt_invoice_paid")
	if resp.Code != http.StatusOK {
		t.Fatalf("payment_succeeded webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err = orgbilling.GetOrgBillingState(ctx, deps, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState after paid invoice: %v", err)
	}
	if state.Plan != orgbilling.PlanTeam || state.SubscriptionStatus != orgbilling.SubscriptionStatusActive {
		t.Fatalf("payment_succeeded should recover active Team state: %+v", state)
	}
	if state.LockedAt.Valid || state.LockReason.Valid || state.GraceUntil.Valid {
		t.Fatalf("payment_succeeded should clear billing action lock: %+v", state)
	}
	if state.PastDueAt.Valid {
		t.Fatalf("payment_succeeded should clear past_due_at: %+v", state)
	}

	current = stripeTestEvent(t, "evt_subscription_deleted", "customer.subscription.deleted", map[string]any{
		"id":                   "sub_test_lifecycle",
		"object":               "subscription",
		"customer":             "cus_test_lifecycle",
		"status":               "canceled",
		"cancel_at_period_end": false,
		"trial_end":            int64(0),
		"canceled_at":          start.Add(2 * time.Hour).Unix(),
		"metadata":             map[string]string{stripebilling.MetadataOrgID: strconv.FormatInt(orgID, 10)},
		"items": map[string]any{"data": []map[string]any{{
			"id":                   "si_test_lifecycle",
			"current_period_start": start.Unix(),
			"current_period_end":   start.Add(30 * 24 * time.Hour).Unix(),
		}}},
	})
	resp = postBillingWebhook(t, mux, "evt_subscription_deleted")
	if resp.Code != http.StatusOK {
		t.Fatalf("subscription deleted webhook status=%d body=%s", resp.Code, resp.Body.String())
	}
	state, err = orgbilling.GetOrgBillingState(ctx, deps, orgID)
	if err != nil {
		t.Fatalf("GetOrgBillingState after cancellation: %v", err)
	}
	if state.Plan != orgbilling.PlanFree || state.SubscriptionStatus != orgbilling.SubscriptionStatusCanceled {
		t.Fatalf("subscription deletion should downgrade to Free canceled state: %+v", state)
	}
	if !state.LockedAt.Valid || !state.LockReason.Valid || state.LockReason.BillingLockReason != billingdb.BillingLockReasonCanceled {
		t.Fatalf("subscription deletion should set canceled lock fields: %+v", state)
	}
}

type fakeStripeRemote struct {
	createCustomerFn    func(context.Context, stripebilling.CustomerInput) (stripebilling.Customer, error)
	createCheckoutFn    func(context.Context, stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error)
	createPortalFn      func(context.Context, stripebilling.PortalInput) (stripebilling.PortalSession, error)
	previewSeatChangeFn func(context.Context, stripebilling.TeamSeatPreviewInput) (stripebilling.TeamSeatPreview, error)
	applySeatChangeFn   func(context.Context, stripebilling.TeamSeatChangeInput) error
	fetchSeatQuantityFn func(context.Context, string) (int64, error)
	updateQuantityFn    func(context.Context, stripebilling.SeatQuantityInput) error
	verifyWebhookFn     func([]byte, string) (stripeapi.Event, error)
}

func (f *fakeStripeRemote) CreateCustomer(ctx context.Context, in stripebilling.CustomerInput) (stripebilling.Customer, error) {
	if f.createCustomerFn == nil {
		return stripebilling.Customer{}, nil
	}
	return f.createCustomerFn(ctx, in)
}

func (f *fakeStripeRemote) CreateCheckoutSession(ctx context.Context, in stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error) {
	if f.createCheckoutFn == nil {
		return stripebilling.CheckoutSession{}, nil
	}
	return f.createCheckoutFn(ctx, in)
}

func (f *fakeStripeRemote) CreatePortalSession(ctx context.Context, in stripebilling.PortalInput) (stripebilling.PortalSession, error) {
	if f.createPortalFn == nil {
		return stripebilling.PortalSession{}, nil
	}
	return f.createPortalFn(ctx, in)
}

func (f *fakeStripeRemote) PreviewTeamSeatChange(ctx context.Context, in stripebilling.TeamSeatPreviewInput) (stripebilling.TeamSeatPreview, error) {
	if f.previewSeatChangeFn == nil {
		return stripebilling.TeamSeatPreview{Currency: "usd", ProrationDate: in.ProrationDate}, nil
	}
	return f.previewSeatChangeFn(ctx, in)
}

func (f *fakeStripeRemote) ApplyTeamSeatChange(ctx context.Context, in stripebilling.TeamSeatChangeInput) error {
	if f.applySeatChangeFn == nil {
		return nil
	}
	return f.applySeatChangeFn(ctx, in)
}

func (f *fakeStripeRemote) FetchSubscriptionItemQuantity(ctx context.Context, subscriptionItemID string) (int64, error) {
	if f.fetchSeatQuantityFn == nil {
		return 0, nil
	}
	return f.fetchSeatQuantityFn(ctx, subscriptionItemID)
}

func (f *fakeStripeRemote) UpdateSubscriptionItemQuantity(ctx context.Context, in stripebilling.SeatQuantityInput) error {
	if f.updateQuantityFn == nil {
		return nil
	}
	return f.updateQuantityFn(ctx, in)
}

func (f *fakeStripeRemote) VerifyWebhook(payload []byte, signatureHeader string) (stripeapi.Event, error) {
	if f.verifyWebhookFn == nil {
		return stripeapi.Event{}, nil
	}
	return f.verifyWebhookFn(payload, signatureHeader)
}

func stripeTestEvent(t *testing.T, id, typ string, raw map[string]any) stripeapi.Event {
	t.Helper()
	return stripeapi.Event{
		ID:         id,
		Type:       stripeapi.EventType(typ),
		APIVersion: "2024-06-20",
		Data:       &stripeapi.EventData{Raw: mustJSONRaw(t, raw)},
	}
}

func mustJSONRaw(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal stripe raw object: %v", err)
	}
	return raw
}

func postBillingWebhook(t *testing.T, mux *chi.Mux, eventID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/stripe/webhook", strings.NewReader(`{"id":"`+eventID+`"}`))
	req.Header.Set("Stripe-Signature", "sig_test")
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	return resp
}

func newOrgBillingMux(t *testing.T, pool *pgxpool.Pool, ownerID int64, remote stripebilling.Remote) *chi.Mux {
	t.Helper()
	return newOrgBillingMuxForUser(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, remote)
}

// newOrgBillingMuxWithPrices is the price-aware variant used by the
// PRO08 cross-kind guard tests. Default mux leaves Team / Pro price
// IDs empty (guard short-circuits); these tests need them populated
// so the guard actually exercises its logic.
func newOrgBillingMuxWithPrices(t *testing.T, pool *pgxpool.Pool, ownerID int64, remote stripebilling.Remote, teamPriceID, proPriceID string) *chi.Mux {
	t.Helper()
	return newOrgBillingMuxFull(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"}, remote, teamPriceID, proPriceID)
}

func newOrgBillingMuxForUser(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser, remote stripebilling.Remote) *chi.Mux {
	t.Helper()
	return newOrgBillingMuxFull(t, pool, viewer, remote, "", "")
}

func newOrgBillingMuxFull(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser, remote stripebilling.Remote, teamPriceID, proPriceID string) *chi.Mux {
	t.Helper()
	tmplFS := fstest.MapFS{
		"_layout.html":                         {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/billing_result.html":             {Data: []byte(`{{ define "page" }}RESULT={{ .Result }};HEADING={{ .Heading }};MESSAGE={{ .Message }};BILLING={{ .BillingPath }}{{ end }}`)},
		"orgs/settings_billing.html":           {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .Notice }}NOTICE={{ . }}{{ end }}{{ with .BillingAlert }}{{ if .Message }}ALERT={{ .Message }}{{ end }}{{ end }}{{ with .Usage.Alert }}{{ if .Message }}USAGE_ALERT={{ .Message }};{{ end }}{{ end }}SEATS={{ .Seats.UsedSeats }}/{{ .Seats.LicensedSeats }}/{{ .Seats.AvailableSeats }}/{{ .Seats.PendingInvites }};{{ range .Usage.Rows }}USAGE={{ .Key }}:{{ .UsedLabel }}/{{ .LimitLabel }}/{{ .PercentLabel }}/{{ .StatusClass }};{{ end }}{{ range .Summary }}{{ if eq .Label "Payment source" }}PAYMENT={{ .Detail }};{{ end }}{{ end }}{{ if .IsSiteAdmin }}DEBUG={{ .Debug.StripeCustomerID }}|{{ .Debug.StripeSubscriptionID }}|{{ .Debug.StripeSubscriptionItemID }}|{{ .Debug.LastWebhookEventID }}|{{ .Debug.LastWebhookStatus }};DRIFT={{ .Debug.SeatDrift.Status }}|{{ .Debug.SeatDrift.LocalLicensedSeats }}|{{ .Debug.SeatDrift.StripeSeats }}|{{ .Debug.SeatDrift.CanRepair }};{{ range .Debug.QuotaOverrides }}OVERRIDE={{ .Kind }}:{{ .Limit }};{{ end }}{{ end }}{{ range .Invoices }}INVOICE={{ .Number }};{{ end }}{{ end }}`)},
		"orgs/settings_billing_licensing.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .Notice }}NOTICE={{ . }}{{ end }}LICENSE={{ .Seats.UsedSeats }}/{{ .Seats.LicensedSeats }}/{{ .Seats.AvailableSeats }}/{{ .Seats.PendingInvites }};{{ range .Consumers }}CONSUMER={{ .Username }}:{{ .RoleLabel }};{{ end }}{{ range .PendingInvites }}PENDING={{ .Target }};{{ end }}ADD={{ .SeatMenu.CanAddSeats }};REMOVE={{ .SeatMenu.CanRemove }};{{ end }}`)},
		"orgs/settings_billing_seats.html":     {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}FORM={{ .Form.Mode }};CURRENT={{ .Form.CurrentSeats }};USED={{ .Form.UsedSeats }};AVAILABLE={{ .Form.AvailableSeats }};NEW={{ .Form.NewTotal }};CAN={{ .Form.CanSubmit }};PRORATION={{ .Form.ProrationLabel }};{{ end }}`)},
		"orgs/people.html":                     {Data: []byte(`{{ define "page" }}{{ with .Notice }}NOTICE={{ . }};{{ end }}{{ if .NoticeActionHref }}ACTION={{ .NoticeActionText }}|{{ .NoticeActionHref }};{{ end }}{{ end }}`)},
		"errors/403.html":                      {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":                      {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":                      {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render:                rr,
		Pool:                  pool,
		BaseURL:               "https://shithub.example",
		BillingEnabled:        true,
		BillingGracePeriod:    14 * 24 * time.Hour,
		Stripe:                remote,
		StripeSuccessURL:      "https://shithub.example/organizations/{org}/billing/success",
		StripeCancelURL:       "https://shithub.example/organizations/{org}/billing/cancel",
		StripePortalReturnURL: "https://shithub.example/organizations/{org}/settings/billing",
		StripeTeamPriceID:     teamPriceID,
		StripeProPriceID:      proPriceID,
	})
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(mux)
	h.MountOrgRoutes(mux)
	h.MountBillingWebhook(mux)
	return mux
}

func insertBillingPendingInvitation(t *testing.T, db *pgxpool.Pool, orgID int64, email string, token []byte) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		INSERT INTO org_invitations (org_id, target_email, role, token_hash, expires_at)
		VALUES ($1, $2, 'member', $3, now() + interval '1 day')
	`, orgID, email, token); err != nil {
		t.Fatalf("insert pending billing invitation: %v", err)
	}
}
