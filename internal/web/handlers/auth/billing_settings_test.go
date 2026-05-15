// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	stripeapi "github.com/stripe/stripe-go/v85"

	userbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
)

func billingPgText(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: true}
}

func billingPgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// PRO06 — user-tier billing settings tests.
//
// These tests share the same server scaffold as the rest of the auth
// suite (newTestServerWithPoolOptions) but flip BillingEnabled and
// hand in a fakeUserStripeRemote so checkout/portal routes register
// and Stripe interactions are observable.

type fakeUserStripeRemote struct {
	createCustomerFn func(context.Context, stripebilling.CustomerInput) (stripebilling.Customer, error)
	createCheckoutFn func(context.Context, stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error)
	createPortalFn   func(context.Context, stripebilling.PortalInput) (stripebilling.PortalSession, error)
	supportsPro      bool
}

func (f *fakeUserStripeRemote) CreateCustomer(ctx context.Context, in stripebilling.CustomerInput) (stripebilling.Customer, error) {
	if f.createCustomerFn == nil {
		return stripebilling.Customer{ID: "cus_default"}, nil
	}
	return f.createCustomerFn(ctx, in)
}

func (f *fakeUserStripeRemote) CreateCheckoutSession(ctx context.Context, in stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error) {
	if f.createCheckoutFn == nil {
		return stripebilling.CheckoutSession{}, nil
	}
	return f.createCheckoutFn(ctx, in)
}

func (f *fakeUserStripeRemote) CreatePortalSession(ctx context.Context, in stripebilling.PortalInput) (stripebilling.PortalSession, error) {
	if f.createPortalFn == nil {
		return stripebilling.PortalSession{}, nil
	}
	return f.createPortalFn(ctx, in)
}

func (f *fakeUserStripeRemote) PreviewTeamSeatChange(context.Context, stripebilling.TeamSeatPreviewInput) (stripebilling.TeamSeatPreview, error) {
	return stripebilling.TeamSeatPreview{}, nil
}

func (f *fakeUserStripeRemote) ApplyTeamSeatChange(context.Context, stripebilling.TeamSeatChangeInput) error {
	return nil
}

func (f *fakeUserStripeRemote) FetchSubscriptionItemQuantity(context.Context, string) (int64, error) {
	return 0, nil
}

func (f *fakeUserStripeRemote) UpdateSubscriptionItemQuantity(_ context.Context, _ stripebilling.SeatQuantityInput) error {
	return nil
}

func (f *fakeUserStripeRemote) VerifyWebhook(_ []byte, _ string) (stripeapi.Event, error) {
	return stripeapi.Event{}, nil
}

func (f *fakeUserStripeRemote) SupportsPro() bool { return f.supportsPro }

// newBillingTestUser signs up + verifies + logs in a fresh user against
// the supplied server. Returns the client (with session cookie) and the
// user's id.
func newBillingTestUser(t *testing.T, srv *httptest.Server, pool *pgxpool.Pool, captor *captureSender, username string) (*client, int64) {
	t.Helper()
	cli := newClient(t, srv)
	mustSignup(t, cli, username, username+"@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()
	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"password":   {"correct horse battery staple"},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE username = $1`, username).Scan(&id); err != nil {
		t.Fatalf("lookup user id: %v", err)
	}
	return cli, id
}

func TestUserBillingSettingsFreeUserShowsUpgradeCTA(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "freeuser")

	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "SUMMARY=Current plan|Free;") {
		t.Errorf("expected Free plan summary, got: %s", s)
	}
	if !strings.Contains(s, "CHECKOUT=true;") {
		t.Errorf("expected CanStartCheckout=true for Free user: %s", s)
	}
	if !strings.Contains(s, "MANAGE=false;") {
		t.Errorf("expected CanManageSubscription=false before customer exists: %s", s)
	}
}

// Regression: a Stripe customer record is created when the user
// starts checkout, well before any payment lands. A user who
// abandoned checkout has a customer_id but no subscription_id and
// must still see "Upgrade to Pro", not "Manage or cancel".
func TestUserBillingSettingsCustomerButNoSubscriptionShowsUpgrade(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "abandoned")
	deps := userbilling.Deps{Pool: pool}
	if _, err := userbilling.SetStripeCustomerForPrincipal(ctx, deps, userbilling.PrincipalForUser(userID), "cus_abandoned"); err != nil {
		t.Fatalf("SetStripeCustomerForPrincipal: %v", err)
	}

	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "SUMMARY=Current plan|Free;") {
		t.Errorf("user with no subscription should still be Free: %s", s)
	}
	if !strings.Contains(s, "CHECKOUT=true;") {
		t.Errorf("expected CanStartCheckout=true (abandoned-checkout user): %s", s)
	}
	if !strings.Contains(s, "MANAGE=false;") {
		t.Errorf("user with customer_id but no subscription must NOT see Manage: %s", s)
	}
}

func TestUserBillingSettingsProUserShowsPlanCardAndPortal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "prouser")
	deps := userbilling.Deps{Pool: pool}
	if _, err := userbilling.SetStripeCustomerForPrincipal(ctx, deps, userbilling.PrincipalForUser(userID), "cus_pro_test"); err != nil {
		t.Fatalf("SetStripeCustomerForPrincipal: %v", err)
	}
	if _, err := billingdb.New().ApplyUserSubscriptionSnapshot(ctx, pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: billingPgText("sub_pro_test"),
		CurrentPeriodStart:   billingPgTime(time.Now().UTC().Add(-time.Hour)),
		CurrentPeriodEnd:     billingPgTime(time.Now().UTC().Add(30 * 24 * time.Hour)),
		LastWebhookEventID:   "evt_pro_test",
	}); err != nil {
		t.Fatalf("ApplyUserSubscriptionSnapshot: %v", err)
	}

	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "SUMMARY=Current plan|Pro;") {
		t.Errorf("expected Pro plan summary: %s", s)
	}
	if !strings.Contains(s, "MANAGE=true;") {
		t.Errorf("Pro user with customer id should have manage=true: %s", s)
	}
	if strings.Contains(s, "cus_pro_test") {
		t.Errorf("non-admin viewer should not see raw stripe customer id: %s", s)
	}
}

func TestUserBillingSettingsCancelAtPeriodEndShowsAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "scheduleuser")
	deps := userbilling.Deps{Pool: pool}
	if _, err := userbilling.SetStripeCustomerForPrincipal(ctx, deps, userbilling.PrincipalForUser(userID), "cus_sched"); err != nil {
		t.Fatalf("SetStripeCustomerForPrincipal: %v", err)
	}
	if _, err := billingdb.New().ApplyUserSubscriptionSnapshot(ctx, pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: billingPgText("sub_sched"),
		CurrentPeriodStart:   billingPgTime(time.Now().UTC().Add(-time.Hour)),
		CurrentPeriodEnd:     billingPgTime(time.Now().UTC().Add(30 * 24 * time.Hour)),
		CancelAtPeriodEnd:    true,
		LastWebhookEventID:   "evt_sched",
	}); err != nil {
		t.Fatalf("ApplyUserSubscriptionSnapshot: %v", err)
	}

	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "ALERT=Pro is scheduled to cancel") {
		t.Errorf("expected cancel-at-period-end alert: %s", s)
	}
}

func TestUserBillingSettingsPastDueShowsAlert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "pastdueuser")
	deps := userbilling.Deps{Pool: pool}
	if _, err := userbilling.SetStripeCustomerForPrincipal(ctx, deps, userbilling.PrincipalForUser(userID), "cus_pastdue"); err != nil {
		t.Fatalf("SetStripeCustomerForPrincipal: %v", err)
	}
	if _, err := userbilling.MarkPastDueForPrincipal(ctx, deps, userbilling.PrincipalForUser(userID), time.Now().UTC().Add(7*24*time.Hour), "evt_pastdue"); err != nil {
		t.Fatalf("MarkPastDueForPrincipal: %v", err)
	}

	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	s := string(body)
	if !strings.Contains(s, "ALERT=Payment failed.") {
		t.Errorf("expected past-due alert: %s", s)
	}
}

func TestUserBillingCheckoutRedirectsToStripeAndPersistsCustomer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := &fakeUserStripeRemote{
		supportsPro: true,
		createCustomerFn: func(_ context.Context, in stripebilling.CustomerInput) (stripebilling.Customer, error) {
			if in.Kind != stripebilling.SubjectKindUser {
				t.Errorf("checkout customer kind = %q, want user", in.Kind)
			}
			return stripebilling.Customer{ID: "cus_checkout"}, nil
		},
		createCheckoutFn: func(_ context.Context, in stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error) {
			if in.Kind != stripebilling.SubjectKindUser {
				t.Errorf("checkout kind = %q, want user", in.Kind)
			}
			if in.CustomerID != "cus_checkout" {
				t.Errorf("checkout customer id = %q", in.CustomerID)
			}
			if !strings.Contains(in.SuccessURL, "/settings/billing/success") {
				t.Errorf("checkout success URL = %q", in.SuccessURL)
			}
			if !strings.Contains(in.CancelURL, "/settings/billing/cancel") {
				t.Errorf("checkout cancel URL = %q", in.CancelURL)
			}
			return stripebilling.CheckoutSession{ID: "cs_user", URL: "https://checkout.stripe.test/user"}, nil
		},
	}
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         fake,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "checkoutuser")
	csrf := cli.extractCSRF(t, "/settings/billing")

	resp := cli.post(t, "/settings/billing/checkout", url.Values{"csrf_token": {csrf}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("checkout status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Location"); got != "https://checkout.stripe.test/user" {
		t.Fatalf("checkout redirect=%q", got)
	}
	state, err := userbilling.GetUserBillingState(ctx, userbilling.Deps{Pool: pool}, userID)
	if err != nil {
		t.Fatalf("GetUserBillingState: %v", err)
	}
	if !state.StripeCustomerID.Valid || state.StripeCustomerID.String != "cus_checkout" {
		t.Fatalf("customer not persisted: %+v", state.StripeCustomerID)
	}
}

func TestUserBillingPortalRedirectsToStripe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fake := &fakeUserStripeRemote{
		supportsPro: true,
		createPortalFn: func(_ context.Context, in stripebilling.PortalInput) (stripebilling.PortalSession, error) {
			if in.CustomerID != "cus_portal_user" {
				t.Errorf("portal customer id = %q", in.CustomerID)
			}
			if !strings.Contains(in.ReturnURL, "/settings/billing") {
				t.Errorf("portal return URL = %q", in.ReturnURL)
			}
			return stripebilling.PortalSession{ID: "bps_user", URL: "https://billing.stripe.test/user"}, nil
		},
	}
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         fake,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "portaluser")
	if _, err := userbilling.SetStripeCustomerForPrincipal(ctx, userbilling.Deps{Pool: pool}, userbilling.PrincipalForUser(userID), "cus_portal_user"); err != nil {
		t.Fatalf("SetStripeCustomerForPrincipal: %v", err)
	}
	csrf := cli.extractCSRF(t, "/settings/billing")

	resp := cli.post(t, "/settings/billing/portal", url.Values{"csrf_token": {csrf}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("portal status=%d body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Location"); got != "https://billing.stripe.test/user" {
		t.Fatalf("portal redirect=%q", got)
	}
}

// When billing isn't configured on the instance, the page itself
// renders (so users see "billing not configured"), but POST/checkout
// returns 404 because the routes aren't registered.
func TestUserBillingStripeDisabledHides404sCheckout(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: false,
	})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "disableduser")

	// The settings page itself stays reachable.
	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/settings/billing status=%d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "CHECKOUT=false;") {
		t.Errorf("disabled instance should not advertise checkout: %s", body)
	}

	// The checkout endpoint is not registered → 404.
	csrf := cli.extractCSRF(t, "/settings/billing")
	resp2 := cli.post(t, "/settings/billing/checkout", url.Values{"csrf_token": {csrf}})
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled checkout status=%d, want 404", resp2.StatusCode)
	}
}

func TestUserBillingRequiresAuth(t *testing.T) {
	t.Parallel()
	srv, _, _ := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli := newClient(t, srv)
	resp := cli.get(t, "/settings/billing")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauth status=%d, want 303 to /login", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login") {
		t.Errorf("expected redirect to /login, got %q", loc)
	}
}

func TestUserBillingResultPagesRender(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		BillingEnabled: true,
		Stripe:         &fakeUserStripeRemote{supportsPro: true},
	})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "resultuser")

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/settings/billing/success", want: "RESULT=success;HEADING=Checkout complete;"},
		{path: "/settings/billing/cancel", want: "RESULT=canceled;HEADING=Checkout canceled;"},
	} {
		resp := cli.get(t, tc.path)
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			t.Errorf("%s status=%d body=%s", tc.path, resp.StatusCode, body)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(string(body), tc.want) {
			t.Errorf("%s missing %q in: %s", tc.path, tc.want, body)
		}
	}
}
