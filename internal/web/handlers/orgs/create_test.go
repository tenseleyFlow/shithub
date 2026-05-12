// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	stripeapi "github.com/stripe/stripe-go/v85"

	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestOrgNewFormShowsPlanSelectionWhenBillingEnabled(t *testing.T) {
	t.Parallel()
	srv, _ := newOrgCreateServer(t, true)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/organizations/new")
	if err != nil {
		t.Fatalf("GET organizations/new: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"PLAN_PAGE",
		"/organizations/new?plan=free",
		"/organizations/new?plan=team",
		"/organizations/new?plan=enterprise",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in body: %s", want, body)
		}
	}
}

func TestOrgPlanSelectionRendersWhenBillingDisabled(t *testing.T) {
	t.Parallel()
	srv, _ := newOrgCreateServer(t, false)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/organizations/plan")
	if err != nil {
		t.Fatalf("GET organizations/plan: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	for _, want := range []string{
		"PLAN_PAGE",
		"CONFIGURED=false",
		"/organizations/new?plan=free",
		"/organizations/new?plan=team",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q in body: %s", want, body)
		}
	}
}

func TestOrgNewFormRendersSetupForSelectedTeamPlan(t *testing.T) {
	t.Parallel()
	srv, _ := newOrgCreateServer(t, true)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/organizations/new?plan=team")
	if err != nil {
		t.Fatalf("GET organizations/new?plan=team: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "FORM_PLAN=team") {
		t.Fatalf("expected team setup form, got: %s", body)
	}
}

func TestOrgNewFormSkipsPlanSelectionWhenBillingDisabled(t *testing.T) {
	t.Parallel()
	srv, _ := newOrgCreateServer(t, false)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().Get(srv.URL + "/organizations/new")
	if err != nil {
		t.Fatalf("GET organizations/new: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "FORM_PLAN=free") {
		t.Fatalf("expected free setup form, got: %s", body)
	}
}

func TestOrgCreateRequiresTermsAcceptance(t *testing.T) {
	t.Parallel()
	srv, _ := newOrgCreateServer(t, false)
	t.Cleanup(srv.Close)

	resp, err := srv.Client().PostForm(srv.URL+"/organizations", url.Values{
		"plan":         {"free"},
		"slug":         {"acme"},
		"display_name": {"Acme"},
	})
	if err != nil {
		t.Fatalf("POST organizations: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "ERROR=You must accept the terms") {
		t.Fatalf("expected terms error, got: %s", body)
	}
}

func TestOrgCreateTeamPlanRedirectsToBillingSettings(t *testing.T) {
	t.Parallel()
	srv, pool := newOrgCreateServer(t, true)
	t.Cleanup(srv.Close)

	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := cli.PostForm(srv.URL+"/organizations", url.Values{
		"plan":          {"team"},
		"slug":          {"acme"},
		"display_name":  {"Acme"},
		"billing_email": {"billing@example.com"},
		"accept_terms":  {"1"},
	})
	if err != nil {
		t.Fatalf("POST organizations: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/organizations/acme/settings/billing?notice=team-created" {
		t.Fatalf("redirect location=%q", got)
	}

	org, err := orgsdb.New().GetOrgBySlug(context.Background(), pool, "acme")
	if err != nil {
		t.Fatalf("GetOrgBySlug: %v", err)
	}
	if org.DisplayName != "Acme" || org.BillingEmail != "billing@example.com" {
		t.Fatalf("unexpected org: %#v", org)
	}
}

func newOrgCreateServer(t *testing.T, billingEnabled bool) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	viewerID := insertOrgAvatarUser(t, pool, "mfwolffe")

	tmplFS := fstest.MapFS{
		"_layout.html":       {Data: []byte(`{{ define "layout" }}<html><body>{{ template "page" . }}</body></html>{{ end }}`)},
		"orgs/new_plan.html": {Data: []byte(`{{ define "page" }}PLAN_PAGE;CONFIGURED={{ .BillingConfigured }}{{ with .Error }};ERROR={{ . }}{{ end }};FREE=/organizations/new?plan=free;TEAM=/organizations/new?plan=team;ENTERPRISE=/organizations/new?plan=enterprise{{ end }}`)},
		"orgs/new.html":      {Data: []byte(`{{ define "page" }}FORM_PLAN={{ .Form.SelectedTier }};ACTION=/organizations{{ with .Error }};ERROR={{ . }}{{ end }}{{ end }}`)},
		"errors/403.html":    {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":    {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":    {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}

	deps := orgsh.Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render:         rr,
		Pool:           pool,
		ObjectStore:    storage.NewMemoryStore(),
		BillingEnabled: billingEnabled,
	}
	if billingEnabled {
		deps.Stripe = noOpStripeRemote{}
	}
	h, err := orgsh.New(deps)
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: viewerID, Username: "mfwolffe"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(r)
	srv := httptest.NewServer(r)
	return srv, pool
}

type noOpStripeRemote struct{}

func (noOpStripeRemote) CreateCustomer(context.Context, stripebilling.CustomerInput) (stripebilling.Customer, error) {
	return stripebilling.Customer{}, nil
}

func (noOpStripeRemote) CreateCheckoutSession(context.Context, stripebilling.CheckoutInput) (stripebilling.CheckoutSession, error) {
	return stripebilling.CheckoutSession{}, nil
}

func (noOpStripeRemote) CreatePortalSession(context.Context, stripebilling.PortalInput) (stripebilling.PortalSession, error) {
	return stripebilling.PortalSession{}, nil
}

func (noOpStripeRemote) UpdateSubscriptionItemQuantity(context.Context, stripebilling.SeatQuantityInput) error {
	return nil
}

func (noOpStripeRemote) VerifyWebhook([]byte, string) (stripeapi.Event, error) {
	return stripeapi.Event{}, nil
}
