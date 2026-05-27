// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/billing"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestRepoAttestationCreateStoresAndDownloadsStatement(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	mux := f.attestationMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alice/public-repo/security/attestations", strings.NewReader(url.Values{
		"statement": {sampleRepoAttestationStatement()},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Location"); got != "/alice/public-repo/security/attestations?created=1" {
		t.Fatalf("redirect location=%q", got)
	}

	rows, err := f.handlers.rq.ListRepoArtifactAttestations(context.Background(), f.pool, reposdb.ListRepoArtifactAttestationsParams{
		RepoID: f.publicRepo.ID,
		Limit:  10,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListRepoArtifactAttestations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("attestation count=%d want 1", len(rows))
	}
	if rows[0].SubjectName != "dist/app.tar.gz" || rows[0].PredicateType != "https://slsa.dev/provenance/v1" {
		t.Fatalf("stored attestation metadata mismatch: %+v", rows[0])
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/security/attestations/"+strconv.FormatInt(rows[0].ID, 10)+"/download", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("download status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/vnd.in-toto+json; charset=utf-8" {
		t.Fatalf("content type=%q", got)
	}
	if got := resp.Header().Get("Content-Disposition"); !strings.Contains(got, `filename="alice-public-repo-attestation-`) {
		t.Fatalf("content disposition=%q", got)
	}
	if !strings.Contains(resp.Body.String(), `"predicateType":"https://slsa.dev/provenance/v1"`) {
		t.Fatalf("download body missing normalized statement: %s", resp.Body.String())
	}

	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/alice/public-repo/security/attestations", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ATT="+strconv.FormatInt(rows[0].ID, 10)+":dist/app.tar.gz:sha256:abcdef123456:https://slsa.dev/provenance/v1") {
		t.Fatalf("page missing attestation row: %s", resp.Body.String())
	}
}

func TestRepoAttestationPrivateOrgRequiresTeam(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	mux := f.attestationMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acme/private-app/security/attestations", strings.NewReader(url.Values{
		"statement": {sampleRepoAttestationStatement()},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "GATE=false") || !strings.Contains(resp.Body.String(), "ERROR=Artifact attestations require Team") {
		t.Fatalf("missing Team gate response: %s", resp.Body.String())
	}
	rows, err := f.handlers.rq.ListRepoArtifactAttestations(context.Background(), f.pool, reposdb.ListRepoArtifactAttestationsParams{
		RepoID: repo.ID,
		Limit:  10,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListRepoArtifactAttestations: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("free private org attestation count=%d want 0", len(rows))
	}
}

func TestRepoAttestationPrivateOrgTeamAllowsCreate(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	upgradeOrgToTeamForAttestations(t, f, orgID)
	mux := f.attestationMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/acme/private-app/security/attestations", strings.NewReader(url.Values{
		"statement": {sampleRepoAttestationStatement()},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusSeeOther {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	rows, err := f.handlers.rq.ListRepoArtifactAttestations(context.Background(), f.pool, reposdb.ListRepoArtifactAttestationsParams{
		RepoID: repo.ID,
		Limit:  10,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListRepoArtifactAttestations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Team private org attestation count=%d want 1", len(rows))
	}
}

func TestRepoAttestationInvalidStatementRendersError(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	mux := f.attestationMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/alice/public-repo/security/attestations", strings.NewReader(url.Values{
		"statement": {`{"subject":[]}`},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "ERROR=attestation statement _type is required") {
		t.Fatalf("missing validation error: %s", resp.Body.String())
	}
}

func (f *repoFixture) attestationMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	mux.Get("/{owner}/{repo}/security/attestations", f.handlers.repoAttestations)
	mux.Post("/{owner}/{repo}/security/attestations", f.handlers.repoAttestationCreate)
	mux.Get("/{owner}/{repo}/security/attestations/{attestationID}/download", f.handlers.repoAttestationDownload)
	return mux
}

func upgradeOrgToTeamForAttestations(t *testing.T, f *repoFixture, orgID int64) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: f.pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_attest_org_" + strconv.FormatInt(orgID, 10),
		StripeSubscriptionItemID: "si_attest_org_" + strconv.FormatInt(orgID, 10),
		CurrentPeriodStart:       now.Add(-time.Hour),
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_attest_org_" + strconv.FormatInt(orgID, 10),
	}); err != nil {
		t.Fatalf("upgrade org to Team: %v", err)
	}
}

func sampleRepoAttestationStatement() string {
	return `{
	  "_type": "https://in-toto.io/Statement/v1",
	  "subject": [{
	    "name": "dist/app.tar.gz",
	    "digest": {"sha256": "ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890ABCDEF1234567890"}
	  }],
	  "predicateType": "https://slsa.dev/provenance/v1",
	  "predicate": {"buildType": "https://shithub.sh/actions"}
	}`
}
