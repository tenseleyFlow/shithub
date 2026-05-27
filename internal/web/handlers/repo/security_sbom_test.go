// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/repos/sbom"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestRepoSBOMGenerateStoresAndDownloadsSPDX(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	seedRepoDependenciesForSBOM(t, f, f.publicRepo.ID, "abc123def456")
	mux := f.sbomMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alice/public-repo/security/sbom/generate", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/public-repo/security/sbom?generated=1" {
		t.Fatalf("redirect location=%q", got)
	}

	export, err := f.handlers.rq.GetRepoSBOMExport(context.Background(), f.pool, reposdb.GetRepoSBOMExportParams{
		RepoID: f.publicRepo.ID,
		Format: sbom.FormatSPDXJSON,
	})
	if err != nil {
		t.Fatalf("GetRepoSBOMExport: %v", err)
	}
	if export.SourceHeadSha != "abc123def456" || export.ByteCount != int64(len(export.Document)) {
		t.Fatalf("export metadata mismatch: %+v", export)
	}
	var doc map[string]any
	if err := json.Unmarshal(export.Document, &doc); err != nil {
		t.Fatalf("export is not JSON: %v", err)
	}
	if doc["spdxVersion"] != "SPDX-2.3" || doc["name"] != "alice/public-repo" {
		t.Fatalf("unexpected SPDX header: %v", doc)
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/security/sbom/download", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("download status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/spdx+json; charset=utf-8" {
		t.Fatalf("content type=%q", got)
	}
	if got := resp.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="alice-public-repo-sbom.spdx.json"`) {
		t.Fatalf("content disposition=%q", got)
	}
	if !strings.Contains(resp.Body.String(), `"github.com/acme/core"`) {
		t.Fatalf("download body missing dependency: %s", resp.Body.String())
	}
}

func TestRepoSBOMPrivateOrgRequiresTeam(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	seedRepoDependenciesForSBOM(t, f, repo.ID, "def456abc123")
	mux := f.sbomMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acme/private-app/security/sbom/generate", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if _, err := f.handlers.rq.GetRepoSBOMExport(context.Background(), f.pool, reposdb.GetRepoSBOMExportParams{
		RepoID: repo.ID,
		Format: sbom.FormatSPDXJSON,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("free private org export err=%v, want no rows", err)
	}
	if !strings.Contains(resp.Body.String(), "GATE=false") || !strings.Contains(resp.Body.String(), "ERROR=SBOM exports require Team") {
		t.Fatalf("missing Team gate response: %s", resp.Body.String())
	}
}

func TestRepoSBOMPrivateOrgTeamAllowsGenerate(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	upgradeOrgToTeamForSBOM(t, f, orgID)
	seedRepoDependenciesForSBOM(t, f, repo.ID, "feedface")
	mux := f.sbomMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acme/private-app/security/sbom/generate", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	export, err := f.handlers.rq.GetRepoSBOMExport(context.Background(), f.pool, reposdb.GetRepoSBOMExportParams{
		RepoID: repo.ID,
		Format: sbom.FormatSPDXJSON,
	})
	if err != nil {
		t.Fatalf("GetRepoSBOMExport: %v", err)
	}
	if export.SourceHeadSha != "feedface" {
		t.Fatalf("source head=%q", export.SourceHeadSha)
	}
}

func TestRepoSBOMPageMarksStaleExport(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	seedRepoDependenciesForSBOM(t, f, f.publicRepo.ID, "oldsha")
	if _, err := f.handlers.rq.UpsertRepoSBOMExport(context.Background(), f.pool, reposdb.UpsertRepoSBOMExportParams{
		RepoID:                        f.publicRepo.ID,
		Format:                        sbom.FormatSPDXJSON,
		SourceHeadSha:                 "oldsha",
		DependencySnapshotGeneratedAt: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Hour), Valid: true},
		Document:                      []byte(`{"spdxVersion":"SPDX-2.3"}`),
		ByteCount:                     int64(len(`{"spdxVersion":"SPDX-2.3"}`)),
		GeneratedBy:                   pgtype.Int8{Int64: f.owner.ID, Valid: true},
	}); err != nil {
		t.Fatalf("seed export: %v", err)
	}
	seedRepoDependenciesForSBOM(t, f, f.publicRepo.ID, "newsha")
	mux := f.sbomMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/security/sbom", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "EXPORT=oldsha:26:true") {
		t.Fatalf("expected stale export marker, body=%s", resp.Body.String())
	}
}

func (f *repoFixture) sbomMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	mux.Get("/{owner}/{repo}/security/sbom", f.handlers.repoSBOM)
	mux.Post("/{owner}/{repo}/security/sbom/generate", f.handlers.repoSBOMGenerate)
	mux.Get("/{owner}/{repo}/security/sbom/download", f.handlers.repoSBOMDownload)
	return mux
}

func seedRepoDependenciesForSBOM(t *testing.T, f *repoFixture, repoID int64, headSHA string) {
	t.Helper()
	if _, err := f.handlers.rq.UpsertRepoDependencySnapshot(context.Background(), f.pool, reposdb.UpsertRepoDependencySnapshotParams{
		RepoID:          repoID,
		DefaultBranch:   "trunk",
		HeadSha:         headSHA,
		ManifestCount:   1,
		DependencyCount: 2,
	}); err != nil {
		t.Fatalf("UpsertRepoDependencySnapshot: %v", err)
	}
	deps := []reposdb.UpsertRepoDependencyParams{
		{
			RepoID:         repoID,
			Ecosystem:      "go",
			PackageName:    "github.com/acme/core",
			PackageVersion: "v1.2.3",
			ManifestPath:   "go.mod",
			Direct:         true,
			PackageManager: "gomod",
			LastSeenSha:    headSHA,
		},
		{
			RepoID:         repoID,
			Ecosystem:      "npm",
			PackageName:    "left-pad",
			PackageVersion: "1.3.0",
			ManifestPath:   "web/package-lock.json",
			Direct:         false,
			PackageManager: "npm",
			LastSeenSha:    headSHA,
		},
	}
	for _, dep := range deps {
		if _, err := f.handlers.rq.UpsertRepoDependency(context.Background(), f.pool, dep); err != nil {
			t.Fatalf("UpsertRepoDependency: %v", err)
		}
	}
}

func upgradeOrgToTeamForSBOM(t *testing.T, f *repoFixture, orgID int64) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: f.pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_sbom_org_" + strconv.FormatInt(orgID, 10),
		StripeSubscriptionItemID: "si_sbom_org_" + strconv.FormatInt(orgID, 10),
		CurrentPeriodStart:       now.Add(-time.Hour),
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_sbom_org_" + strconv.FormatInt(orgID, 10),
	}); err != nil {
		t.Fatalf("upgrade org to Team: %v", err)
	}
}
