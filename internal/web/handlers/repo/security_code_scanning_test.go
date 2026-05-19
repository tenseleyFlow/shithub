// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/billing"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func TestCodeScanningUploadUpsertsAlerts(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	mux := f.codeScanningMux(f.owner.ID, f.owner.Username)

	for i := 0; i < 2; i++ {
		resp := httptest.NewRecorder()
		req := sarifUploadRequest("/alice/public-repo/security/code-scanning/upload", sampleSARIF())
		mux.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("POST #%d status=%d body=%s", i+1, resp.Code, resp.Body.String())
		}
	}

	if uploads := codeScanningUploadCount(t, f, f.publicRepo.ID); uploads != 2 {
		t.Fatalf("uploads=%d, want 2", uploads)
	}
	alerts, err := f.handlers.rq.ListCodeScanningAlertsForRepo(context.Background(), f.pool, reposdb.ListCodeScanningAlertsForRepoParams{
		RepoID: f.publicRepo.ID,
		Limit:  10,
		Offset: 0,
	})
	if err != nil {
		t.Fatalf("ListCodeScanningAlertsForRepo: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%d, want deduped 1", len(alerts))
	}
	alert := alerts[0]
	if alert.RuleID != "G401" || alert.Path != "internal/app/main.go" || alert.Severity != "high" {
		t.Fatalf("alert not normalized: %+v", alert)
	}
}

func TestCodeScanningPrivateOrgRequiresTeam(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	mux := f.codeScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := sarifUploadRequest("/acme/private-app/security/code-scanning/upload", sampleSARIF())
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := codeScanningUploadCount(t, f, repo.ID); got != 0 {
		t.Fatalf("free private org upload count=%d, want 0", got)
	}
	if !strings.Contains(resp.Body.String(), "ERROR=Code scanning SARIF uploads require Team") {
		t.Fatalf("missing Team gate error: %s", resp.Body.String())
	}
}

func TestCodeScanningPrivateOrgTeamAllowsUpload(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	upgradeOrgToTeamForCodeScanning(t, f, orgID)
	mux := f.codeScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := sarifUploadRequest("/acme/private-app/security/code-scanning/upload", sampleSARIF())
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("POST status=%d body=%s", resp.Code, resp.Body.String())
	}
	if got := codeScanningUploadCount(t, f, repo.ID); got != 1 {
		t.Fatalf("Team private org upload count=%d, want 1", got)
	}
}

func (f *repoFixture) codeScanningMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	mux.Get("/{owner}/{repo}/security/code-scanning", f.handlers.repoCodeScanning)
	mux.Post("/{owner}/{repo}/security/code-scanning/upload", f.handlers.repoCodeScanningUpload)
	return mux
}

func sarifUploadRequest(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/sarif+json")
	return req
}

func sampleSARIF() string {
	return `{
	  "version": "2.1.0",
	  "runs": [{
	    "automationDetails": {"id": "go-security"},
	    "tool": {"driver": {
	      "name": "gosec",
	      "guid": "tool-guid",
	      "rules": [{
	        "id": "G401",
	        "name": "weak crypto",
	        "shortDescription": {"text": "Weak cryptography"}
	      }]
	    }},
	    "results": [{
	      "ruleId": "G401",
	      "message": {"text": "Use of weak crypto primitive"},
	      "locations": [{"physicalLocation": {
	        "artifactLocation": {"uri": "./internal/app/main.go"},
	        "region": {"startLine": 42, "startColumn": 7}
	      }}],
	      "partialFingerprints": {"primaryLocationLineHash": "stable-fp"},
	      "properties": {"security-severity": "8.1"}
	    }]
	  }]
	}`
}

func codeScanningUploadCount(t *testing.T, f *repoFixture, repoID int64) int64 {
	t.Helper()
	var got int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM code_scanning_uploads WHERE repo_id = $1`, repoID,
	).Scan(&got); err != nil {
		t.Fatalf("count code scanning uploads: %v", err)
	}
	return got
}

func upgradeOrgToTeamForCodeScanning(t *testing.T, f *repoFixture, orgID int64) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(orgID, 10)
	if _, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: f.pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_code_org_" + suffix,
		StripeSubscriptionItemID: "si_code_org_" + suffix,
		CurrentPeriodStart:       now.Add(-time.Hour),
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_code_org_" + suffix,
	}); err != nil {
		t.Fatalf("upgrade org to Team: %v", err)
	}
}
