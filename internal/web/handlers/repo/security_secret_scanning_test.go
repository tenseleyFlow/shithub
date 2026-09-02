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
	"github.com/jackc/pgx/v5/pgtype"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-10c — Security tab secret-scanning page + allowlist
// handlers. Worker-level scanning is exercised in
// internal/worker/jobs/secret_scan_history_test.go; here we test the
// HTTP surface.

// TestSecretScanning_AllowlistAddAndRemove pins the CRUD round-trip:
// POST adds an entry; the allowlist sweep flips any matching open
// finding to status=allowlisted; the remove endpoint deletes the row.
func TestSecretScanning_AllowlistAddAndRemove(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	upgradeRepoFixtureOwnerToProForSecretScan(t, f)
	seedFinding(t, f, "aws-access-key-id", "config.json", 3, "REDACTED-EXCERPT")
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	csrf := middleware.CSRFTokenForRequest(httptest.NewRequest(http.MethodGet, "/", nil))
	_ = csrf // CSRF middleware is bypassed via WithCurrentUserForTest in the mux
	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost,
		"/alice/public-repo/security/secret-scanning/allowlist",
		url.Values{
			"pattern": {"aws-access-key-id"},
			"path":    {"config.json"},
			"reason":  {"deploy script, not a real key"},
		})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", resp.Code, resp.Body.String())
	}

	// Verify the allowlist row + the finding status flip.
	count := allowlistCount(t, f, f.publicRepo.ID)
	if count != 1 {
		t.Errorf("allowlist count: got %d, want 1", count)
	}
	if status := findingStatus(t, f, f.publicRepo.ID); status != "allowlisted" {
		t.Errorf("finding status: got %q, want allowlisted", status)
	}

	// Remove it.
	var allowlistID int64
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM secret_scan_allowlist WHERE repo_id = $1 LIMIT 1`, f.publicRepo.ID,
	).Scan(&allowlistID); err != nil {
		t.Fatalf("lookup allowlist id: %v", err)
	}
	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost,
		"/alice/public-repo/security/secret-scanning/allowlist/"+strconv.FormatInt(allowlistID, 10)+"/remove",
		url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("remove status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if c := allowlistCount(t, f, f.publicRepo.ID); c != 0 {
		t.Errorf("allowlist count after remove: got %d, want 0", c)
	}
}

// TestSecretScanning_AllowlistRejectsEmptyFields confirms basic input
// validation.
func TestSecretScanning_AllowlistRejectsEmptyFields(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	upgradeRepoFixtureOwnerToProForSecretScan(t, f)
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost,
		"/alice/public-repo/security/secret-scanning/allowlist",
		url.Values{"pattern": {""}, "path": {"config.json"}})
	mux.ServeHTTP(resp, req)
	if c := allowlistCount(t, f, f.publicRepo.ID); c != 0 {
		t.Errorf("empty-pattern submit should not insert: got %d", c)
	}
}

// TestSecretScanning_RunScanEnforceBlocksFree pins the Pro-gate
// behaviour on the "Run scan now" button: enforce mode refuses, no
// job is enqueued. The gate is scoped to *private* personal repos —
// public repositories get supported-pattern scanning as an SP26
// baseline — so the private fixture repo is the one under test.
func TestSecretScanning_RunScanEnforceBlocksFree(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserSecretScanHistory: true})
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/private-repo/security/secret-scanning/scan", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if jobs := jobsEnqueuedCount(t, f, "secret_scan:history"); jobs != 0 {
		t.Errorf("enforce-mode Free should not enqueue a scan job, got %d", jobs)
	}
}

// TestSecretScanning_RunScanPublicRepoEnqueuesWithoutPro pins the other
// half of the SP26 contract: public repositories keep baseline scanning
// even with the user enforce flag on.
func TestSecretScanning_RunScanPublicRepoEnqueuesWithoutPro(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserSecretScanHistory: true})
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/security/secret-scanning/scan", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if jobs := jobsEnqueuedCount(t, f, "secret_scan:history"); jobs != 1 {
		t.Errorf("public repo should enqueue exactly 1 job, got %d", jobs)
	}
}

// TestSecretScanning_RunScanProEnqueues verifies a Pro owner clicking
// the button queues exactly one job on a private repo.
func TestSecretScanning_RunScanProEnqueues(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserSecretScanHistory: true})
	upgradeRepoFixtureOwnerToProForSecretScan(t, f)
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/private-repo/security/secret-scanning/scan", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if jobs := jobsEnqueuedCount(t, f, "secret_scan:history"); jobs != 1 {
		t.Errorf("Pro owner should enqueue exactly 1 job, got %d", jobs)
	}
}

func TestSecretScanning_PrivateOrgRequiresTeam(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)

	gate := f.handlers.repoSecretScanGate(context.Background(), repo, "acme")
	if gate.Allowed {
		t.Fatalf("private Free org repo should not allow on-demand secret scanning")
	}
	if gate.FeatureKey != "secret_scanning" {
		t.Fatalf("feature key=%q, want secret_scanning", gate.FeatureKey)
	}
	if gate.UpgradeHref != "/organizations/acme/settings/billing" || gate.UpgradeText != "Upgrade to Team" {
		t.Fatalf("upgrade affordance = %q %q", gate.UpgradeHref, gate.UpgradeText)
	}
}

func TestSecretScanning_PrivateOrgTeamAllowsScan(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	upgradeOrgToTeamForSecretScan(t, f, orgID)

	gate := f.handlers.repoSecretScanGate(context.Background(), repo, "acme")
	if !gate.Allowed {
		t.Fatalf("private Team org repo should allow on-demand secret scanning: %+v", gate)
	}
}

func TestSecretScanning_PublicOrgAllowedWithoutTeam(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "public-app", reposdb.RepoVisibilityPublic)

	gate := f.handlers.repoSecretScanGate(context.Background(), repo, "acme")
	if !gate.Allowed {
		t.Fatalf("public org repo should keep baseline secret scanning: %+v", gate)
	}
}

func TestSecretScanning_BypassApproveAndDeny(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	first := seedBypassRequest(t, f, f.publicRepo.ID, "github-token", "config/secrets.env", strings.Repeat("a", 40))
	second := seedBypassRequest(t, f, f.publicRepo.ID, "github-token", "config/other.env", strings.Repeat("b", 40))
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/alice/public-repo/security/secret-scanning", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if got, want := resp.Body.String(), "REQ=github-token:config/secrets.env:"+strings.Repeat("a", 40)+":1:pending;"; !strings.Contains(got, want) {
		t.Fatalf("GET body missing %q in %s", want, got)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost,
		"/alice/public-repo/security/secret-scanning/bypass/"+strconv.FormatInt(first.ID, 10)+"/approve",
		url.Values{"approved_for_hours": {"48"}, "review_note": {"fixture approval"}})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("approve status: got %d body=%s", resp.Code, resp.Body.String())
	}
	approved := getBypassRequest(t, f, f.publicRepo.ID, first.ID)
	if approved.Status != secretscandb.SecretScanBypassStatusApproved || !approved.ApprovedUntil.Valid {
		t.Fatalf("approved row status=%s approved_until=%v", approved.Status, approved.ApprovedUntil.Valid)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost,
		"/alice/public-repo/security/secret-scanning/bypass/"+strconv.FormatInt(second.ID, 10)+"/deny",
		url.Values{"review_note": {"still a secret"}})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("deny status: got %d body=%s", resp.Code, resp.Body.String())
	}
	denied := getBypassRequest(t, f, f.publicRepo.ID, second.ID)
	if denied.Status != secretscandb.SecretScanBypassStatusDenied || denied.ApprovedUntil.Valid {
		t.Fatalf("denied row status=%s approved_until=%v", denied.Status, denied.ApprovedUntil.Valid)
	}
}

func TestSecretScanning_PrivateOrgBypassRequiresTeam(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t)
	orgID := f.insertOwnedOrg(t, "acme")
	repo := f.insertOrgRepo(t, orgID, "private-app", reposdb.RepoVisibilityPrivate)
	reqRow := seedBypassRequest(t, f, repo.ID, "github-token", "config/private.env", strings.Repeat("c", 40))
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/acme/private-app/security/secret-scanning", nil)
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("GET free status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := resp.Body.String(); !strings.Contains(got, "BYPASS=false;") || strings.Contains(got, "config/private.env") {
		t.Fatalf("free private org should gate exact bypass rows, body=%s", got)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost,
		"/acme/private-app/security/secret-scanning/bypass/"+strconv.FormatInt(reqRow.ID, 10)+"/approve",
		url.Values{"approved_for_hours": {"24"}})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("approve free status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := getBypassRequest(t, f, repo.ID, reqRow.ID).Status; got != secretscandb.SecretScanBypassStatusPending {
		t.Fatalf("free private org changed status to %s, want pending", got)
	}

	upgradeOrgToTeamForSecretScan(t, f, orgID)
	resp = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/acme/private-app/security/secret-scanning", nil)
	mux.ServeHTTP(resp, req)
	if got := resp.Body.String(); !strings.Contains(got, "REQ=github-token:config/private.env:"+strings.Repeat("c", 40)+":1:pending;") {
		t.Fatalf("team private org should reveal request row, body=%s", got)
	}

	resp = httptest.NewRecorder()
	req = newFormRequest(http.MethodPost,
		"/acme/private-app/security/secret-scanning/bypass/"+strconv.FormatInt(reqRow.ID, 10)+"/approve",
		url.Values{"approved_for_hours": {"24"}})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("approve team status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if got := getBypassRequest(t, f, repo.ID, reqRow.ID).Status; got != secretscandb.SecretScanBypassStatusApproved {
		t.Fatalf("team private org status=%s, want approved", got)
	}
}

// securityScanningMux wires the secret-scanning routes against the
// repoFixture in a minimal chi mux with the test viewer pinned to
// the owner.
func (f *repoFixture) securityScanningMux(userID int64, username string) http.Handler {
	mux := chi.NewRouter()
	mux.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			viewer := middleware.CurrentUser{ID: userID, Username: username}
			next.ServeHTTP(w, r.WithContext(middleware.WithCurrentUserForTest(r.Context(), viewer)))
		})
	})
	mux.Get("/{owner}/{repo}/security/secret-scanning", f.handlers.repoSecretScanning)
	mux.Post("/{owner}/{repo}/security/secret-scanning/scan", f.handlers.repoSecretScanningRunScan)
	mux.Post("/{owner}/{repo}/security/secret-scanning/allowlist", f.handlers.repoSecretScanningAllowlistAdd)
	mux.Post("/{owner}/{repo}/security/secret-scanning/allowlist/{id}/remove", f.handlers.repoSecretScanningAllowlistRemove)
	mux.Post("/{owner}/{repo}/security/secret-scanning/bypass/{id}/approve", f.handlers.repoSecretScanningBypassApprove)
	mux.Post("/{owner}/{repo}/security/secret-scanning/bypass/{id}/deny", f.handlers.repoSecretScanningBypassDeny)
	return mux
}

// upgradeRepoFixtureOwnerToProForSecretScan promotes the fixture owner
// to Pro with a unique stripe sub id (parallel-test-safe).
func upgradeRepoFixtureOwnerToProForSecretScan(t *testing.T, f *repoFixture) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(f.owner.ID, 10)
	if _, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), f.pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               f.owner.ID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_ssh_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_ssh_pro_" + suffix,
	}); err != nil {
		t.Fatalf("upgrade to Pro: %v", err)
	}
}

func upgradeOrgToTeamForSecretScan(t *testing.T, f *repoFixture, orgID int64) {
	t.Helper()
	now := time.Now().UTC()
	if _, err := orgbilling.ApplySubscriptionSnapshot(context.Background(), orgbilling.Deps{Pool: f.pool}, orgbilling.SubscriptionSnapshot{
		OrgID:                    orgID,
		Plan:                     orgbilling.PlanTeam,
		Status:                   orgbilling.SubscriptionStatusActive,
		StripeSubscriptionID:     "sub_secret_org_" + strconv.FormatInt(orgID, 10),
		StripeSubscriptionItemID: "si_secret_org_" + strconv.FormatInt(orgID, 10),
		CurrentPeriodStart:       now.Add(-time.Hour),
		CurrentPeriodEnd:         now.Add(30 * 24 * time.Hour),
		LastWebhookEventID:       "evt_secret_org_" + strconv.FormatInt(orgID, 10),
	}); err != nil {
		t.Fatalf("upgrade org to Team: %v", err)
	}
}

func seedFinding(t *testing.T, f *repoFixture, pattern, path string, line int, excerpt string) {
	t.Helper()
	if _, err := f.pool.Exec(
		context.Background(),
		`INSERT INTO secret_scan_findings (repo_id, pattern, path, line_no, excerpt, first_seen_oid, last_seen_oid)
		 VALUES ($1, $2, $3, $4, $5, 'deadbeef', 'deadbeef')`,
		f.publicRepo.ID, pattern, path, line, excerpt,
	); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
}

func seedBypassRequest(t *testing.T, f *repoFixture, repoID int64, pattern, path, commit string) secretscandb.SecretScanBypassRequest {
	t.Helper()
	row, err := secretscandb.New().UpsertSecretScanBypassRequest(context.Background(), f.pool, secretscandb.UpsertSecretScanBypassRequestParams{
		RepoID:        repoID,
		Pattern:       pattern,
		Path:          path,
		CommitOid:     commit,
		LineNo:        1,
		RequestedBy:   pgtype.Int8{Int64: f.owner.ID, Valid: true},
		RequestReason: "test fixture",
	})
	if err != nil {
		t.Fatalf("UpsertSecretScanBypassRequest: %v", err)
	}
	return row
}

func getBypassRequest(t *testing.T, f *repoFixture, repoID, id int64) secretscandb.SecretScanBypassRequest {
	t.Helper()
	row, err := secretscandb.New().GetSecretScanBypassRequest(context.Background(), f.pool, secretscandb.GetSecretScanBypassRequestParams{
		ID:     id,
		RepoID: repoID,
	})
	if err != nil {
		t.Fatalf("GetSecretScanBypassRequest: %v", err)
	}
	return row
}

func allowlistCount(t *testing.T, f *repoFixture, repoID int64) int64 {
	t.Helper()
	var got int64
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM secret_scan_allowlist WHERE repo_id = $1`, repoID,
	).Scan(&got); err != nil {
		t.Fatalf("count allowlist: %v", err)
	}
	return got
}

func findingStatus(t *testing.T, f *repoFixture, repoID int64) string {
	t.Helper()
	var status string
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT status::text FROM secret_scan_findings WHERE repo_id = $1 LIMIT 1`, repoID,
	).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return status
}

func jobsEnqueuedCount(t *testing.T, f *repoFixture, kind string) int64 {
	t.Helper()
	var got int64
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = $1`, kind,
	).Scan(&got); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return got
}
