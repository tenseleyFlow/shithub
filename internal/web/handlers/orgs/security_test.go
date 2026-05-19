// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	orgsh "github.com/tenseleyFlow/shithub/internal/web/handlers/orgs"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

func TestOrgSecurityOverviewRendersTeamDependencyAlerts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	if _, err := orgbilling.ApplySubscriptionSnapshot(ctx, orgbilling.Deps{Pool: pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_sec",
		StripeSubscriptionItemID: "si_sec",
		LastWebhookEventID:       "evt_sec",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
	seedOrgDependencyAlert(t, pool, orgID)

	mux := newOrgSecurityMux(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"})
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/acme/security", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /acme/security status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		"SUMMARY=1/1/0;",
		"ALERT=app:example.test/vulnerable:high;",
		"REPO=app:1;",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q: %s", want, body)
		}
	}
}

func TestOrgSecurityOverviewRequiresOrgMembership(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	strangerID := insertOrgAvatarUser(t, pool, "stranger")
	insertOrgAvatarOrg(t, pool, ownerID, "acme")

	mux := newOrgSecurityMux(t, pool, middleware.CurrentUser{ID: strangerID, Username: "stranger"})
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/acme/security", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("GET /acme/security status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestOrgSecurityOverviewFreeOrgDoesNotRenderAlertDetails(t *testing.T) {
	t.Parallel()
	pool := dbtest.NewTestDB(t)
	ownerID := insertOrgAvatarUser(t, pool, "owner")
	orgID := insertOrgAvatarOrg(t, pool, ownerID, "acme")
	seedOrgDependencyAlert(t, pool, orgID)

	mux := newOrgSecurityMux(t, pool, middleware.CurrentUser{ID: ownerID, Username: "owner"})
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/acme/security", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("GET /acme/security status=%d body=%s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "LOCK=Security overview features require Team billing") {
		t.Fatalf("upgrade lock missing from body: %s", body)
	}
	if strings.Contains(body, "example.test/vulnerable") {
		t.Fatalf("locked org leaked alert details: %s", body)
	}
}

func seedOrgDependencyAlert(t *testing.T, pool *pgxpool.Pool, orgID int64) {
	t.Helper()
	ctx := context.Background()
	rq := reposdb.New()
	repo, err := rq.CreateRepo(ctx, pool, reposdb.CreateRepoParams{
		OwnerOrgID:      pgtype.Int8{Int64: orgID, Valid: true},
		Name:            "app",
		Description:     "Dependency test app",
		Visibility:      reposdb.RepoVisibilityPrivate,
		DefaultBranch:   "trunk",
		PrimaryLanguage: pgtype.Text{String: "Go", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if _, err := rq.UpsertRepoDependencySnapshot(ctx, pool, reposdb.UpsertRepoDependencySnapshotParams{
		RepoID:          repo.ID,
		DefaultBranch:   "trunk",
		HeadSha:         "deadbeef",
		ManifestCount:   1,
		DependencyCount: 1,
	}); err != nil {
		t.Fatalf("UpsertRepoDependencySnapshot: %v", err)
	}
	if _, err := rq.UpsertRepoDependency(ctx, pool, reposdb.UpsertRepoDependencyParams{
		RepoID:         repo.ID,
		Ecosystem:      "go",
		PackageName:    "example.test/vulnerable",
		PackageVersion: "v1.2.3",
		ManifestPath:   "go.mod",
		Scope:          "runtime",
		Direct:         true,
		PackageManager: "gomod",
		Source:         "go.mod",
		LastSeenSha:    "deadbeef",
	}); err != nil {
		t.Fatalf("UpsertRepoDependency: %v", err)
	}
	if _, err := rq.UpsertDependencyAdvisory(ctx, pool, reposdb.UpsertDependencyAdvisoryParams{
		Source:          "test-fixture",
		ExternalID:      "GHSA-org-security",
		Ecosystem:       "go",
		PackageName:     "example.test/vulnerable",
		AffectedRange:   "v1.2.3",
		PatchedVersions: "v1.2.4",
		Severity:        "high",
		Summary:         "Fixture vulnerability",
		Description:     "Only used by org security tests.",
		ReferenceUrls:   []byte("[]"),
	}); err != nil {
		t.Fatalf("UpsertDependencyAdvisory: %v", err)
	}
	if err := rq.RefreshDependencyAlertsForRepo(ctx, pool, repo.ID); err != nil {
		t.Fatalf("RefreshDependencyAlertsForRepo: %v", err)
	}
}

func newOrgSecurityMux(t *testing.T, pool *pgxpool.Pool, viewer middleware.CurrentUser) *chi.Mux {
	t.Helper()
	tmplFS := fstest.MapFS{
		"_layout.html":       {Data: []byte(`{{ define "layout" }}{{ template "page" . }}{{ end }}`)},
		"orgs/security.html": {Data: []byte(`{{ define "page" }}{{ if .Locked }}LOCK={{ .UpgradeBanner.Message }};{{ else }}SUMMARY={{ .Summary.OpenAlertCount }}/{{ .Summary.DependencyCount }}/{{ .Summary.RepositoryAdvisoryCount }};{{ range .Alerts }}ALERT={{ .RepoName }}:{{ .PackageName }}:{{ .Severity }};{{ end }}{{ range .Repositories }}REPO={{ .RepoName }}:{{ .DependencyCount }};{{ end }}{{ end }}{{ end }}`)},
		"errors/404.html":    {Data: []byte(`{{ define "page" }}404{{ end }}`)},
		"errors/500.html":    {Data: []byte(`{{ define "page" }}500{{ end }}`)},
	}
	rr, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New: %v", err)
	}
	h, err := orgsh.New(orgsh.Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Render: rr,
		Pool:   pool,
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
	h.MountOrgRoutes(mux)
	return mux
}
