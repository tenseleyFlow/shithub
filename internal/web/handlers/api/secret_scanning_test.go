// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func TestSecretScanningAPIListsMetadataWithoutSecretMaterial(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	ownerID := seedRepoCreatorUser(t, pool, "alice")
	orgID := seedSecretScanningAPIOrg(t, pool, ownerID, "acme")
	activateSecretScanningTeamPlan(t, pool, orgID)
	repo := seedSecretScanningAPIRepo(t, pool, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	seedSecretScanningAPIRows(t, pool, repo.ID, ownerID)
	token := mintRunnerAPIPAT(t, pool, ownerID, string(pat.ScopeRepoRead))

	rr := secretScanningAPIRequest(t, router, token, "/api/v1/repos/acme/private-app/secret-scanning/alerts")
	if rr.Code != http.StatusOK {
		t.Fatalf("alerts status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, forbidden := range []string{"ghp_SECRET_SHOULD_NOT_LEAK", "[REDACTED]", "excerpt"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("alerts response leaked secret material marker %q: %s", forbidden, body)
		}
	}
	var alerts []struct {
		ID                    int64  `json:"id"`
		State                 string `json:"state"`
		Status                string `json:"status"`
		SecretType            string `json:"secret_type"`
		SecretTypeDisplayName string `json:"secret_type_display_name"`
		ProviderSlug          string `json:"provider_slug"`
		PatternCategory       string `json:"pattern_category"`
		Validity              string `json:"validity"`
		ValidityCheck         struct {
			SupportedByGitHub   bool   `json:"supported_by_github"`
			SupportedByInstance bool   `json:"supported_by_instance"`
			Status              string `json:"status"`
		} `json:"validity_check"`
		ProviderNotification           string `json:"provider_notification"`
		ProviderNotificationCapability struct {
			SupportedByGitHub   bool   `json:"supported_by_github"`
			SupportedByInstance bool   `json:"supported_by_instance"`
			Status              string `json:"status"`
		} `json:"provider_notification_capability"`
		Path      string `json:"path"`
		Line      int32  `json:"line"`
		CommitSHA string `json:"commit_sha"`
		HTMLURL   string `json:"html_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &alerts); err != nil {
		t.Fatalf("decode alerts: %v; body=%s", err, body)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts len=%d want 1: %+v", len(alerts), alerts)
	}
	if alerts[0].State != "open" || alerts[0].Status != "open" || alerts[0].SecretType != "github_token" || alerts[0].SecretTypeDisplayName != "GitHub token" {
		t.Fatalf("unexpected alert: %+v", alerts[0])
	}
	if alerts[0].ProviderSlug != "github" || alerts[0].PatternCategory != "provider" {
		t.Fatalf("unexpected provider capability: %+v", alerts[0])
	}
	if !alerts[0].ValidityCheck.SupportedByGitHub || !alerts[0].ProviderNotificationCapability.SupportedByGitHub {
		t.Fatalf("expected GitHub reference support flags on github-token: %+v", alerts[0])
	}
	if alerts[0].Validity != "unsupported" || alerts[0].ValidityCheck.Status != "unsupported" || alerts[0].ValidityCheck.SupportedByInstance {
		t.Fatalf("validity must be truthfully unsupported: %+v", alerts[0])
	}
	if alerts[0].ProviderNotification != "unsupported" || alerts[0].ProviderNotificationCapability.Status != "unsupported" || alerts[0].ProviderNotificationCapability.SupportedByInstance {
		t.Fatalf("provider notification must be truthfully unsupported: %+v", alerts[0])
	}
	if alerts[0].Path != "config/secrets.env" || alerts[0].Line != 7 {
		t.Fatalf("unexpected alert location: %+v", alerts[0])
	}
	if alerts[0].CommitSHA == "" || !strings.Contains(alerts[0].HTMLURL, "/acme/private-app/security/secret-scanning") {
		t.Fatalf("missing commit/html URL fields: %+v", alerts[0])
	}

	rr = secretScanningAPIRequest(t, router, token, "/api/v1/repos/acme/private-app/secret-scanning/allowlist")
	if rr.Code != http.StatusOK {
		t.Fatalf("allowlist status=%d body=%s", rr.Code, rr.Body.String())
	}
	var allowlist struct {
		TotalCount int `json:"total_count"`
		Allowlist  []struct {
			Pattern   string `json:"pattern"`
			Path      string `json:"path"`
			CreatedBy *struct {
				Login string `json:"login"`
			} `json:"created_by"`
		} `json:"allowlist"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &allowlist); err != nil {
		t.Fatalf("decode allowlist: %v; body=%s", err, rr.Body.String())
	}
	if allowlist.TotalCount != 1 || len(allowlist.Allowlist) != 1 {
		t.Fatalf("allowlist count mismatch: %+v", allowlist)
	}
	if allowlist.Allowlist[0].Pattern != "Stripe key" || allowlist.Allowlist[0].Path != "fixtures/test.env" {
		t.Fatalf("allowlist row mismatch: %+v", allowlist.Allowlist[0])
	}
	if allowlist.Allowlist[0].CreatedBy == nil || allowlist.Allowlist[0].CreatedBy.Login != "alice" {
		t.Fatalf("allowlist actor missing: %+v", allowlist.Allowlist[0].CreatedBy)
	}

	rr = secretScanningAPIRequest(t, router, token, "/api/v1/repos/acme/private-app/secret-scanning/bypass-requests")
	if rr.Code != http.StatusOK {
		t.Fatalf("bypass status=%d body=%s", rr.Code, rr.Body.String())
	}
	var bypass struct {
		TotalCount     int `json:"total_count"`
		BypassRequests []struct {
			Pattern     string `json:"pattern"`
			Path        string `json:"path"`
			CommitSHA   string `json:"commit_sha"`
			Status      string `json:"status"`
			RequestedBy *struct {
				Login string `json:"login"`
			} `json:"requested_by"`
		} `json:"bypass_requests"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &bypass); err != nil {
		t.Fatalf("decode bypass: %v; body=%s", err, rr.Body.String())
	}
	if bypass.TotalCount != 1 || len(bypass.BypassRequests) != 1 {
		t.Fatalf("bypass count mismatch: %+v", bypass)
	}
	if bypass.BypassRequests[0].Status != "pending" || bypass.BypassRequests[0].CommitSHA == "" {
		t.Fatalf("bypass row mismatch: %+v", bypass.BypassRequests[0])
	}
	if bypass.BypassRequests[0].RequestedBy == nil || bypass.BypassRequests[0].RequestedBy.Login != "alice" {
		t.Fatalf("bypass actor missing: %+v", bypass.BypassRequests[0].RequestedBy)
	}

	rr = secretScanningAPIRequest(t, router, token, "/api/v1/repos/acme/private-app/secret-scanning/status")
	if rr.Code != http.StatusOK {
		t.Fatalf("status status=%d body=%s", rr.Code, rr.Body.String())
	}
	var status struct {
		Enabled                        bool  `json:"enabled"`
		TotalAlertCount                int64 `json:"total_alert_count"`
		OpenAlertCount                 int64 `json:"open_alert_count"`
		AllowlistCount                 int   `json:"allowlist_count"`
		BypassControlsAvailable        bool  `json:"bypass_controls_available"`
		BypassRequestCount             int   `json:"bypass_request_count"`
		RawSecretMaterialIncluded      bool  `json:"raw_secret_material_included"`
		ValidityChecksAvailable        bool  `json:"validity_checks_available"`
		ProviderNotificationsAvailable bool  `json:"provider_notifications_available"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v; body=%s", err, rr.Body.String())
	}
	if !status.Enabled || status.TotalAlertCount != 1 || status.OpenAlertCount != 1 || status.AllowlistCount != 1 || status.BypassRequestCount != 1 {
		t.Fatalf("status summary mismatch: %+v", status)
	}
	if !status.BypassControlsAvailable || status.RawSecretMaterialIncluded {
		t.Fatalf("status security flags mismatch: %+v", status)
	}
	if status.ValidityChecksAvailable || status.ProviderNotificationsAvailable {
		t.Fatalf("provider egress flags must stay disabled until a real integration ships: %+v", status)
	}
}

func TestSecretScanningAPIFreePrivateOrgDoesNotRevealDetails(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	ownerID := seedRepoCreatorUser(t, pool, "alice")
	orgID := seedSecretScanningAPIOrg(t, pool, ownerID, "freeco")
	repo := seedSecretScanningAPIRepo(t, pool, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	seedSecretScanningAPIRows(t, pool, repo.ID, ownerID)
	token := mintRunnerAPIPAT(t, pool, ownerID, string(pat.ScopeRepoRead))

	for _, path := range []string{
		"/api/v1/repos/freeco/private-app/secret-scanning/status",
		"/api/v1/repos/freeco/private-app/secret-scanning/alerts",
		"/api/v1/repos/freeco/private-app/secret-scanning/allowlist",
		"/api/v1/repos/freeco/private-app/secret-scanning/bypass-requests",
	} {
		rr := secretScanningAPIRequest(t, router, token, path)
		if rr.Code != http.StatusPaymentRequired {
			t.Fatalf("%s status=%d want 402; body=%s", path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		for _, forbidden := range []string{"config/secrets.env", "fixtures/test.env", "ghp_SECRET_SHOULD_NOT_LEAK"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s leaked private secret scan metadata %q: %s", path, forbidden, body)
			}
		}
	}
}

func TestSecretScanningAPIRequiresRepoSettingsAccess(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	ownerID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	orgID := seedSecretScanningAPIOrg(t, pool, ownerID, "acme")
	activateSecretScanningTeamPlan(t, pool, orgID)
	repo := seedSecretScanningAPIRepo(t, pool, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	seedSecretScanningAPIRows(t, pool, repo.ID, ownerID)

	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))
	rr := secretScanningAPIRequest(t, router, bobToken, "/api/v1/repos/acme/private-app/secret-scanning/alerts")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("stranger status=%d want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func secretScanningAPIRequest(t *testing.T, router http.Handler, token, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

func seedSecretScanningAPIOrg(t *testing.T, pool *pgxpool.Pool, ownerID int64, slug string) int64 {
	t.Helper()
	org, err := orgs.Create(context.Background(), orgs.Deps{Pool: pool}, orgs.CreateParams{
		Slug:            slug,
		DisplayName:     slug,
		CreatedByUserID: ownerID,
	})
	if err != nil {
		t.Fatalf("orgs.Create: %v", err)
	}
	return org.ID
}

func seedSecretScanningAPIRepo(t *testing.T, pool *pgxpool.Pool, orgID int64, name string, visibility reposdb.RepoVisibility) reposdb.Repo {
	t.Helper()
	repo, err := reposdb.New().CreateRepo(context.Background(), pool, reposdb.CreateRepoParams{
		OwnerOrgID:    pgtype.Int8{Int64: orgID, Valid: true},
		Name:          name,
		Description:   "secret scanning api fixture",
		Visibility:    visibility,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return repo
}

func activateSecretScanningTeamPlan(t *testing.T, pool *pgxpool.Pool, orgID int64) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := billing.ApplySubscriptionSnapshot(context.Background(), billing.Deps{Pool: pool}, billing.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     billing.PlanTeam,
		Status:                   billing.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_secret_scan_api",
		StripeSubscriptionItemID: "si_secret_scan_api",
		CurrentPeriodStart:       now,
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_secret_scan_api",
	}); err != nil {
		t.Fatalf("ApplySubscriptionSnapshot: %v", err)
	}
}

func seedSecretScanningAPIRows(t *testing.T, pool *pgxpool.Pool, repoID, actorID int64) {
	t.Helper()
	q := secretscandb.New()
	if _, err := q.UpsertSecretScanFinding(context.Background(), pool, secretscandb.UpsertSecretScanFindingParams{
		RepoID:       repoID,
		Pattern:      "github-token",
		Path:         "config/secrets.env",
		LineNo:       7,
		Excerpt:      "ghp_SECRET_SHOULD_NOT_LEAK",
		FirstSeenOid: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatalf("UpsertSecretScanFinding: %v", err)
	}
	if _, err := q.InsertSecretScanAllowlist(context.Background(), pool, secretscandb.InsertSecretScanAllowlistParams{
		RepoID:    repoID,
		Pattern:   "Stripe key",
		Path:      "fixtures/test.env",
		Reason:    "test fixture",
		CreatedBy: pgtype.Int8{Int64: actorID, Valid: true},
	}); err != nil {
		t.Fatalf("InsertSecretScanAllowlist: %v", err)
	}
	if _, err := q.UpsertSecretScanBypassRequest(context.Background(), pool, secretscandb.UpsertSecretScanBypassRequestParams{
		RepoID:        repoID,
		Pattern:       "GitHub token",
		Path:          "config/secrets.env",
		CommitOid:     "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		LineNo:        7,
		RequestedBy:   pgtype.Int8{Int64: actorID, Valid: true},
		RequestReason: "false positive in fixture",
	}); err != nil {
		t.Fatalf("UpsertSecretScanBypassRequest: %v", err)
	}
}
