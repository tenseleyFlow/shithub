// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
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

func TestOrgSecretPatternsTeamCRUD(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.ApplySubscriptionSnapshot(context.Background(), orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_secret_patterns",
		StripeSubscriptionItemID: "si_secret_patterns",
		LastWebhookEventID:       "evt_secret_patterns",
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
	req := newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/security/secret-patterns", url.Values{
		"name":          {"internal-token"},
		"description":   {"Internal token"},
		"pattern":       {`shithub_custom_[A-Za-z0-9]{12,}`},
		"min_match_len": {"16"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST custom pattern status=%d body=%s", resp.Code, resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/security/secret-patterns", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET custom pattern status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "PATTERN=internal-token:true;") {
		t.Fatalf("custom pattern missing from list: %s", got)
	}
}

func TestOrgSecretPatternsBlocksAndHidesRowsWithoutTeamEntitlement(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO secret_scan_custom_patterns (org_id, name, description, pattern, min_match_len)
		VALUES ($1, 'internal-token', 'Internal token', 'shithub_custom_[A-Za-z0-9]{12,}', 16)`,
		orgID,
	); err != nil {
		t.Fatalf("seed custom pattern: %v", err)
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
	req := httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/security/secret-patterns", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET free custom patterns status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); strings.Contains(got, "internal-token") {
		t.Fatalf("free custom patterns page revealed stored pattern: %s", got)
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/security/secret-patterns", url.Values{
		"name":          {"second-token"},
		"pattern":       {`shithub_custom_[A-Za-z0-9]{12,}`},
		"min_match_len": {"16"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("POST free custom pattern status=%d body=%s", resp.Code, resp.Body.String())
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM secret_scan_custom_patterns WHERE org_id = $1`,
		orgID,
	).Scan(&count); err != nil {
		t.Fatalf("query custom pattern count: %v", err)
	}
	if count != 1 {
		t.Fatalf("custom pattern count=%d, want only seeded row", count)
	}
}

func TestOrgSecuritySettingsRequiredTwoFactorRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
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
	req := httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/security", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET org security status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "SETTING=false") {
		t.Fatalf("default required 2fa setting missing: %s", got)
	}

	resp = httptest.NewRecorder()
	req = newOrgFormRequest(http.MethodPost, "/organizations/acme/settings/security", url.Values{
		"require_two_factor": {"on"},
	})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST org security status=%d body=%s", resp.Code, resp.Body.String())
	}
	var requireTwoFactor bool
	if err := pool.QueryRow(ctx, `SELECT require_two_factor FROM org_security_settings WHERE org_id = $1`, orgID).Scan(&requireTwoFactor); err != nil {
		t.Fatalf("query org security settings: %v", err)
	}
	if !requireTwoFactor {
		t.Fatal("require_two_factor=false, want true")
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM auth_audit_log WHERE action = 'org_required_2fa_updated' AND target_type = 'org' AND target_id = $1`, orgID).Scan(&auditCount); err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("audit count=%d, want 1", auditCount)
	}
}

func TestOrgAuditLogShowsOrgAndOwnedRepoEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	repoID := insertOrgAuditOrgRepo(t, pool, orgID, "project")
	otherOwnerID := insertOrgAvatarUser(t, pool, "other")
	otherRepoID := insertOrgAuditUserRepo(t, pool, otherOwnerID, "outside")
	for _, row := range []struct {
		action     string
		targetType string
		targetID   int64
	}{
		{"org_required_2fa_updated", "org", orgID},
		{"repo_visibility_changed", "repo", repoID},
		{"repo_visibility_changed", "repo", otherRepoID},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO auth_audit_log (actor_id, action, target_type, target_id, meta)
			 VALUES ($1, $2, $3, $4, '{"test":true}'::jsonb)`,
			ownerID, row.action, row.targetType, row.targetID,
		); err != nil {
			t.Fatalf("insert audit row %s: %v", row.action, err)
		}
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
	req := httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/audit-log", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET org audit status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "ROW=org_required_2fa_updated|org|") {
		t.Fatalf("org audit row missing: %s", body)
	}
	if !strings.Contains(body, "ROW=repo_visibility_changed|repo|"+strconv.FormatInt(repoID, 10)+";") {
		t.Fatalf("owned repo audit row missing: %s", body)
	}
	if strings.Contains(body, "ROW=repo_visibility_changed|repo|"+strconv.FormatInt(otherRepoID, 10)+";") {
		t.Fatalf("unowned repo audit row leaked: %s", body)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/audit-log?action=org_", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("filtered GET org audit status=%d body=%s", resp.Code, resp.Body.String())
	}
	body = resp.Body.String()
	if !strings.Contains(body, "ROW=org_required_2fa_updated|org|") {
		t.Fatalf("filtered org audit row missing: %s", body)
	}
	if strings.Contains(body, "ROW=repo_visibility_changed|repo|"+strconv.FormatInt(repoID, 10)+";") {
		t.Fatalf("action prefix filter included repo row: %s", body)
	}
	if !strings.Contains(body, "FILTERS=action=org_;") {
		t.Fatalf("filter query not preserved for export/pagination: %s", body)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/organizations/acme/settings/audit-log/export?action=repo_", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("export org audit status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("export content type=%q", got)
	}
	if got := resp.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="acme-audit-log.csv"`) {
		t.Fatalf("export content disposition=%q", got)
	}
	body = resp.Body.String()
	if !strings.Contains(body, "created_at,actor_id,action,target_type,target_id,meta") {
		t.Fatalf("export header missing: %s", body)
	}
	if !strings.Contains(body, ",repo_visibility_changed,repo,"+strconv.FormatInt(repoID, 10)+",") {
		t.Fatalf("owned repo row missing from export: %s", body)
	}
	if strings.Contains(body, "org_required_2fa_updated") {
		t.Fatalf("action filter included org row in export: %s", body)
	}
	if strings.Contains(body, ",repo_visibility_changed,repo,"+strconv.FormatInt(otherRepoID, 10)+",") {
		t.Fatalf("unowned repo row leaked in export: %s", body)
	}
}

func newOrgActionsHandler(t *testing.T, pool *pgxpool.Pool) *orgsh.Handlers {
	t.Helper()
	tmplFS := fstest.MapFS{
		"_layout.html":                       {Data: []byte(`{{ define "layout" }}{{ template "page" . }}{{ end }}`)},
		"orgs/settings_profile.html":         {Data: []byte(`{{ define "page" }}profile{{ end }}`)},
		"orgs/settings_secrets.html":         {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .WritesDisabledMessage }}LOCK={{ . }}{{ end }}{{ range .Secrets }}SECRET={{ .Name }};{{ end }}{{ range .Variables }}VAR={{ .Name }}:{{ .Value }};{{ end }}{{ end }}`)},
		"orgs/settings_secret_patterns.html": {Data: []byte(`{{ define "page" }}{{ with .Error }}ERROR={{ . }}{{ end }}{{ with .WritesDisabledMessage }}LOCK={{ . }}{{ end }}{{ range .Patterns }}PATTERN={{ .Name }}:{{ .Enabled }};{{ end }}{{ end }}`)},
		"orgs/settings_security.html":        {Data: []byte(`{{ define "page" }}{{ with .Notice }}NOTICE={{ . }}{{ end }}SETTING={{ .Settings.RequireTwoFactor }}{{ end }}`)},
		"orgs/settings_audit.html":           {Data: []byte(`{{ define "page" }}{{ range .Rows }}ROW={{ .Action }}|{{ .TargetType }}|{{ if .TargetID.Valid }}{{ .TargetID.Int64 }}{{ end }};{{ end }}FILTERS={{ .Filters }};{{ end }}`)},
		"errors/403.html":                    {Data: []byte(`{{ define "page" }}403{{ end }}`)},
		"errors/404.html":                    {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":                    {Data: []byte(`{{ define "page" }}500{{ end }}`)},
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

func insertOrgAuditOrgRepo(t *testing.T, db orgsdb.DBTX, orgID int64, name string) int64 {
	t.Helper()
	var repoID int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO repos (owner_org_id, name, visibility, default_branch)
		 VALUES ($1, $2, 'public', 'trunk')
		 RETURNING id`,
		orgID, name,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert org repo: %v", err)
	}
	return repoID
}

func insertOrgAuditUserRepo(t *testing.T, db orgsdb.DBTX, userID int64, name string) int64 {
	t.Helper()
	var repoID int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO repos (owner_user_id, name, visibility, default_branch)
		 VALUES ($1, $2, 'public', 'trunk')
		 RETURNING id`,
		userID, name,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert user repo: %v", err)
	}
	return repoID
}
