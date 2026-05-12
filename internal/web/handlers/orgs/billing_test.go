// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"encoding/json"
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
	"github.com/jackc/pgx/v5/pgxpool"
	stripeapi "github.com/stripe/stripe-go/v85"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
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
	receipt, err := billingdb.New().GetWebhookEventReceipt(ctx, pool, "evt_sub_active")
	if err != nil {
		t.Fatalf("GetWebhookEventReceipt: %v", err)
	}
	if !receipt.ProcessedAt.Valid || receipt.ProcessingAttempts != 1 {
		t.Fatalf("unexpected receipt after first processing: %+v", receipt)
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
	if state.LockedAt.Valid || state.LockReason.Valid || state.GraceUntil.Valid {
		t.Fatalf("payment_succeeded should clear billing action lock: %+v", state)
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
	createCustomerFn func(context.Context, stripebilling.CustomerInput) (stripebilling.Customer, error)
	createCheckoutFn func(context.Context, stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error)
	createPortalFn   func(context.Context, stripebilling.PortalInput) (stripebilling.PortalSession, error)
	updateQuantityFn func(context.Context, stripebilling.SeatQuantityInput) error
	verifyWebhookFn  func([]byte, string) (stripeapi.Event, error)
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
	tmplFS := fstest.MapFS{
		"_layout.html":               {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/settings_billing.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .Notice }}NOTICE={{ . }}{{ end }}{{ range .Invoices }}INVOICE={{ .Number }};{{ end }}{{ end }}`)},
		"errors/403.html":            {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":            {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":            {Data: []byte(`{{ define "page" }}500{{ end }}`)},
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
	})
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: ownerID, Username: "owner"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(mux)
	h.MountBillingWebhook(mux)
	return mux
}
