// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestRepoSecurityAdvisoryCreateDraftRecordsEvent(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	form := securityAdvisoryFormValues()
	form.Set("ghsa_id", "GHSA-ABCD-1234-5678")
	form.Set("reference_urls", "https://example.com/advisory\nhttps://example.com/fix")

	req := repoRouteFormRequest(http.MethodPost, "/alice/public-repo/security/advisories", f.owner.Username, f.publicRepo.Name, "", viewerFor(f.owner), form)
	rw := httptest.NewRecorder()
	f.handlers.repoSecurityAdvisoryCreate(rw, req)

	if rw.Code != http.StatusSeeOther {
		t.Fatalf("status %d, want 303: %s", rw.Code, rw.Body.String())
	}
	if got, want := rw.Header().Get("Location"), "/alice/public-repo/security/advisories/GHSA-ABCD-1234-5678"; got != want {
		t.Fatalf("redirect=%q, want %q", got, want)
	}
	row := f.mustGetSecurityAdvisory(t, f.publicRepo.ID, "GHSA-ABCD-1234-5678")
	if row.State != "draft" || row.CreatedBy.Int64 != f.owner.ID || !row.CreatedBy.Valid {
		t.Fatalf("advisory state/creator = %s/%+v", row.State, row.CreatedBy)
	}
	if got := f.countSecurityAdvisoryEvents(t, row.ID, "created"); got != 1 {
		t.Fatalf("created events=%d, want 1", got)
	}
	if got := string(row.ReferenceUrls); !strings.Contains(got, "https://example.com/advisory") || !strings.Contains(got, "https://example.com/fix") {
		t.Fatalf("reference urls not persisted: %s", got)
	}
}

func TestRepoSecurityAdvisoryStateTransitions(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	row := f.createSecurityAdvisory(t, f.publicRepo.ID, "SHSA-TEST-STATE", "draft", "**impact**")

	for _, action := range []string{"publish", "withdraw", "archive", "reopen"} {
		form := url.Values{"action": {action}}
		req := repoRouteFormRequest(http.MethodPost, "/alice/public-repo/security/advisories/SHSA-TEST-STATE/state", f.owner.Username, f.publicRepo.Name, row.Identifier, viewerFor(f.owner), form)
		rw := httptest.NewRecorder()
		f.handlers.repoSecurityAdvisoryState(rw, req)
		if rw.Code != http.StatusSeeOther {
			t.Fatalf("action %s status %d, want 303: %s", action, rw.Code, rw.Body.String())
		}
	}

	got := f.mustGetSecurityAdvisory(t, f.publicRepo.ID, row.Identifier)
	if got.State != "draft" {
		t.Fatalf("final state=%s, want draft", got.State)
	}
	for _, eventType := range []string{"published", "withdrawn", "archived", "reopened"} {
		if count := f.countSecurityAdvisoryEvents(t, row.ID, eventType); count != 1 {
			t.Fatalf("%s events=%d, want 1", eventType, count)
		}
	}
}

func TestRepoSecurityAdvisoryPublishAndWithdrawRefreshDependencyAlerts(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	ctx := context.Background()
	f.upsertCurrentDependency(t, f.publicRepo.ID, "go", "github.com/example/pkg", "v1.2.2")
	row := f.createSecurityAdvisory(t, f.publicRepo.ID, "SHSA-LOCAL-DEPS", "draft", "Upgrade to `v1.2.3`.")

	alerts, err := reposdb.New().ListOpenDependencyAlertsForRepo(ctx, f.pool, f.publicRepo.ID)
	if err != nil {
		t.Fatalf("ListOpenDependencyAlertsForRepo pre-publish: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("draft advisory opened alerts: %+v", alerts)
	}

	form := url.Values{"action": {"publish"}}
	req := repoRouteFormRequest(http.MethodPost, "/alice/public-repo/security/advisories/SHSA-LOCAL-DEPS/state", f.owner.Username, f.publicRepo.Name, row.Identifier, viewerFor(f.owner), form)
	rw := httptest.NewRecorder()
	f.handlers.repoSecurityAdvisoryState(rw, req)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("publish status %d, want 303: %s", rw.Code, rw.Body.String())
	}

	alerts, err = reposdb.New().ListOpenDependencyAlertsForRepo(ctx, f.pool, f.publicRepo.ID)
	if err != nil {
		t.Fatalf("ListOpenDependencyAlertsForRepo post-publish: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("open alerts after publish = %+v, want one", alerts)
	}
	if alerts[0].Source != repoSecurityAdvisoryDependencySource(f.publicRepo.ID) ||
		alerts[0].ExternalID != "SHSA-LOCAL-DEPS" ||
		alerts[0].PatchedVersions != "1.2.3" {
		t.Fatalf("alert mismatch after publish: %+v", alerts[0])
	}

	form = url.Values{"action": {"withdraw"}}
	req = repoRouteFormRequest(http.MethodPost, "/alice/public-repo/security/advisories/SHSA-LOCAL-DEPS/state", f.owner.Username, f.publicRepo.Name, row.Identifier, viewerFor(f.owner), form)
	rw = httptest.NewRecorder()
	f.handlers.repoSecurityAdvisoryState(rw, req)
	if rw.Code != http.StatusSeeOther {
		t.Fatalf("withdraw status %d, want 303: %s", rw.Code, rw.Body.String())
	}

	alerts, err = reposdb.New().ListOpenDependencyAlertsForRepo(ctx, f.pool, f.publicRepo.ID)
	if err != nil {
		t.Fatalf("ListOpenDependencyAlertsForRepo post-withdraw: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("withdrawn advisory left open alerts: %+v", alerts)
	}
	if got := f.countDependencyAlertsByStatus(t, f.publicRepo.ID, "resolved"); got != 1 {
		t.Fatalf("resolved dependency alerts=%d, want 1", got)
	}
}

func TestRepoSecurityAdvisoryDetailSanitizesMarkdown(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.createSecurityAdvisory(t, f.publicRepo.ID, "SHSA-SAFE", "published", "<script>alert(1)</script>\n\n**patched**")

	req := repoRouteRequestWithIdentifier(http.MethodGet, "/alice/public-repo/security/advisories/SHSA-SAFE", f.owner.Username, f.publicRepo.Name, "SHSA-SAFE", anonymousViewer())
	rw := httptest.NewRecorder()
	f.handlers.repoSecurityAdvisoryDetail(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if strings.Contains(body, "<script>") || strings.Contains(body, "&lt;script") {
		t.Fatalf("unsafe script leaked into advisory detail: %s", body)
	}
	if !strings.Contains(body, "<strong>patched</strong>") {
		t.Fatalf("markdown was not rendered: %s", body)
	}
}

func TestRepoSecurityAdvisoryListDoesNotLeakDraftsToReaders(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	f.createSecurityAdvisory(t, f.publicRepo.ID, "SHSA-DRAFT", "draft", "draft details")
	f.createSecurityAdvisory(t, f.publicRepo.ID, "SHSA-PUBLISHED", "published", "published details")

	req := repoRouteRequest(http.MethodGet, "/alice/public-repo/security/advisories", f.owner.Username, f.publicRepo.Name, anonymousViewer())
	rw := httptest.NewRecorder()
	f.handlers.repoSecurityAdvisories(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rw.Code, rw.Body.String())
	}
	body := rw.Body.String()
	if strings.Contains(body, "SHSA-DRAFT") || strings.Contains(body, "COUNTS=1/") {
		t.Fatalf("draft advisory leaked to reader: %s", body)
	}
	if !strings.Contains(body, "ADV=SHSA-PUBLISHED:published") || !strings.Contains(body, "COUNTS=0/1/0/0") {
		t.Fatalf("published advisory missing or counts wrong: %s", body)
	}
}

func TestRepoSecurityAdvisoryPrivateOrgRequiresTeamForCreate(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	orgRepo, err := reposdb.New().CreateRepo(context.Background(), f.pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          "private-org-repo",
		Description:   "",
		Visibility:    reposdb.RepoVisibilityPrivate,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo org private: %v", err)
	}

	req := repoRouteFormRequest(http.MethodPost, "/acme/private-org-repo/security/advisories", "acme", orgRepo.Name, "", viewerFor(f.owner), securityAdvisoryFormValues())
	rw := httptest.NewRecorder()
	f.handlers.repoSecurityAdvisoryCreate(rw, req)

	if rw.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 gate render: %s", rw.Code, rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "ALLOWED=false") || !strings.Contains(rw.Body.String(), "Repository security advisories require Team") {
		t.Fatalf("missing Team gate message: %s", rw.Body.String())
	}
	if got := f.countSecurityAdvisories(t, orgRepo.ID); got != 0 {
		t.Fatalf("advisories created=%d, want 0", got)
	}
}

func securityAdvisoryFormValues() url.Values {
	return url.Values{
		"summary":             {"Package vulnerable to unsafe input"},
		"severity":            {"high"},
		"affected_ecosystem":  {"go"},
		"affected_package":    {"github.com/example/pkg"},
		"vulnerable_versions": {"< 1.2.3"},
		"patched_versions":    {"1.2.3"},
		"description":         {"Upgrade to 1.2.3."},
	}
}

func repoRouteFormRequest(method, target, owner, repoName, identifier string, viewer middleware.CurrentUser, form url.Values) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("owner", owner)
	rctx.URLParams.Add("repo", repoName)
	if identifier != "" {
		rctx.URLParams.Add("identifier", identifier)
	}
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return withViewer(req, viewer)
}

func repoRouteRequestWithIdentifier(method, target, owner, repoName, identifier string, viewer middleware.CurrentUser) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("owner", owner)
	rctx.URLParams.Add("repo", repoName)
	rctx.URLParams.Add("identifier", identifier)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	return withViewer(req, viewer)
}

func (f *repoFixture) createSecurityAdvisory(t *testing.T, repoID int64, identifier, state, description string) reposdb.RepoSecurityAdvisory {
	t.Helper()
	row, err := reposdb.New().CreateRepoSecurityAdvisory(context.Background(), f.pool, reposdb.CreateRepoSecurityAdvisoryParams{
		RepoID:             repoID,
		Identifier:         identifier,
		State:              state,
		Severity:           "high",
		Summary:            "Unsafe package input",
		Description:        description,
		AffectedEcosystem:  "go",
		AffectedPackage:    "github.com/example/pkg",
		VulnerableVersions: "v1.2.2",
		PatchedVersions:    "1.2.3",
		ReferenceUrls:      []byte(`[]`),
		CreatedBy:          pgtype.Int8{Int64: f.owner.ID, Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateRepoSecurityAdvisory: %v", err)
	}
	return row
}

func (f *repoFixture) mustGetSecurityAdvisory(t *testing.T, repoID int64, identifier string) reposdb.RepoSecurityAdvisory {
	t.Helper()
	row, err := reposdb.New().GetRepoSecurityAdvisoryByIdentifier(context.Background(), f.pool, reposdb.GetRepoSecurityAdvisoryByIdentifierParams{
		RepoID:     repoID,
		Identifier: identifier,
	})
	if err != nil {
		t.Fatalf("GetRepoSecurityAdvisoryByIdentifier: %v", err)
	}
	return row
}

func (f *repoFixture) countSecurityAdvisoryEvents(t *testing.T, advisoryID int64, eventType string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repo_security_advisory_events WHERE advisory_id = $1 AND event_type = $2`,
		advisoryID, eventType).Scan(&count); err != nil {
		t.Fatalf("count security advisory events: %v", err)
	}
	return count
}

func (f *repoFixture) countSecurityAdvisories(t *testing.T, repoID int64) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repo_security_advisories WHERE repo_id = $1`,
		repoID).Scan(&count); err != nil {
		t.Fatalf("count security advisories: %v", err)
	}
	return count
}

func (f *repoFixture) upsertCurrentDependency(t *testing.T, repoID int64, ecosystem, packageName, version string) {
	t.Helper()
	if _, err := reposdb.New().UpsertRepoDependency(context.Background(), f.pool, reposdb.UpsertRepoDependencyParams{
		RepoID:         repoID,
		Ecosystem:      ecosystem,
		PackageName:    packageName,
		PackageVersion: version,
		ManifestPath:   "go.mod",
		Scope:          "runtime",
		Direct:         true,
		PackageManager: "gomod",
		Source:         "go.mod",
		LastSeenSha:    "deadbeef",
	}); err != nil {
		t.Fatalf("UpsertRepoDependency: %v", err)
	}
}

func (f *repoFixture) countDependencyAlertsByStatus(t *testing.T, repoID int64, status string) int {
	t.Helper()
	var count int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM repo_dependency_alerts WHERE repo_id = $1 AND status = $2`,
		repoID, status).Scan(&count); err != nil {
		t.Fatalf("count dependency alerts by status: %v", err)
	}
	return count
}
