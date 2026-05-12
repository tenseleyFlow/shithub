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

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestOrgActionsSettingsSecretAndVariableCRUD(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.ApplySubscriptionSnapshot(context.Background(), orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_test",
		StripeSubscriptionItemID: "si_test",
		LastWebhookEventID:       "evt_test",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	h := newOrgActionsHandler(t, pool)
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: ownerID, Username: "owner"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(mux)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/secrets/actions", url.Values{
		"name":  {"ORG_TOKEN"},
		"value": {"super-secret"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST org secret status=%d body=%s", resp.Code, resp.Body.String())
	}
	var ciphertext []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT ciphertext FROM workflow_secrets WHERE org_id = $1 AND name = $2`,
		orgID, "ORG_TOKEN").Scan(&ciphertext); err != nil {
		t.Fatalf("query org secret: %v", err)
	}
	if strings.Contains(string(ciphertext), "super-secret") {
		t.Fatal("plaintext appeared in org workflow_secrets.ciphertext")
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/variables/actions", url.Values{
		"name":  {"REGISTRY"},
		"value": {"registry.internal"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST org variable status=%d body=%s", resp.Code, resp.Body.String())
	}
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/variables/actions", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET org variable status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "VAR=REGISTRY:registry.internal;") {
		t.Fatalf("org variable missing from list: %s", got)
	}
}

func TestOrgActionsSettingsBlocksWritesWithoutTeamEntitlement(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	h := newOrgActionsHandler(t, pool)
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: ownerID, Username: "owner"}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	h.MountCreate(mux)

	resp := httptest.NewRecorder()
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/secrets/actions", url.Values{
		"name":  {"ORG_TOKEN"},
		"value": {"super-secret"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("POST org secret status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "require Team billing") {
		t.Fatalf("upgrade message missing from body: %s", got)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM workflow_secrets WHERE org_id = $1`,
		orgID,
	).Scan(&count); err != nil {
		t.Fatalf("query org secret count: %v", err)
	}
	if count != 0 {
		t.Fatalf("workflow secrets count=%d, want 0", count)
	}
}

func newOrgActionsHandler(t *testing.T, pool *pgxpool.Pool) *orgsh.Handlers {
	t.Helper()
	tmplFS := fstest.MapFS{
		"_layout.html":               {Data: []byte(`{{ define "layout" }}{{ template "page" . }}{{ end }}`)},
		"orgs/settings_profile.html": {Data: []byte(`{{ define "page" }}profile{{ end }}`)},
		"orgs/settings_secrets.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .WritesDisabledMessage }}LOCK={{ . }}{{ end }}{{ range .Secrets }}SECRET={{ .Name }};{{ end }}{{ range .Variables }}VAR={{ .Name }}:{{ .Value }};{{ end }}{{ end }}`)},
		"errors/403.html":            {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":            {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":            {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	key, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	box, err := secretbox.FromBytes(key)
	if err != nil {
		t.Fatalf("FromBytes: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render:      rr,
		Pool:        pool,
		ObjectStore: storage.NewMemoryStore(),
		SecretBox:   box,
	})
	if err != nil {
		t.Fatalf("orgsh.New: %v", err)
	}
	return h
}

func newOrgFormRequest(method, target string, form url.Values) *http.Request {
	body := ""
	if form != nil {
		body = form.Encode()
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}
