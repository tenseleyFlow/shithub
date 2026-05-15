// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
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
// job is enqueued.
func TestSecretScanning_RunScanEnforceBlocksFree(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserSecretScanHistory: true})
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/security/secret-scanning/scan", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if jobs := jobsEnqueuedCount(t, f, "secret_scan:history"); jobs != 0 {
		t.Errorf("enforce-mode Free should not enqueue a scan job, got %d", jobs)
	}
}

// TestSecretScanning_RunScanProEnqueues verifies a Pro owner clicking
// the button queues exactly one job.
func TestSecretScanning_RunScanProEnqueues(t *testing.T) {
	t.Parallel()
	f := newRepoFixtureWithEnforce(t, config.EnforceConfig{UserSecretScanHistory: true})
	upgradeRepoFixtureOwnerToProForSecretScan(t, f)
	mux := f.securityScanningMux(f.owner.ID, f.owner.Username)

	resp := httptest.NewRecorder()
	req := newFormRequest(http.MethodPost, "/alice/public-repo/security/secret-scanning/scan", url.Values{})
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.Code, resp.Body.String())
	}
	if jobs := jobsEnqueuedCount(t, f, "secret_scan:history"); jobs != 1 {
		t.Errorf("Pro owner should enqueue exactly 1 job, got %d", jobs)
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
