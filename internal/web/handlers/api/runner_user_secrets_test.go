// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	billingdb "github.com/tenseleyFlow/shithub/internal/billing/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// PRO-EXT01-12b: verifies the runner-side resolveVisibleSecretsFromDB
// merges user-scope rows for user-owned repos, that repo-scope rows
// shadow user-scope rows with the same name, and that the enforce flag
// filters user-scope rows out for non-Pro accounts.
//
// In-package (not api_test) because resolveVisibleSecretsFromDB is
// unexported and the contract this test pins is critical enough to be
// worth poking the internal seam directly.

func TestResolveVisibleSecrets_MergesUserScopeForUserOwnedRepos(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	box := mustBox(t)

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")

	// Seed a USER-scope secret + a REPO-scope secret with a shadowing name.
	mustUserSecret(t, pool, box, userID, "USER_KEY", []byte("user-value"))
	mustUserSecret(t, pool, box, userID, "SHARED", []byte("user-shared"))
	mustRepoSecret(t, pool, box, repoID, "REPO_KEY", []byte("repo-value"))
	mustRepoSecret(t, pool, box, repoID, "SHARED", []byte("repo-shared"))

	got, err := h.resolveVisibleSecretsFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["USER_KEY"] != "user-value" {
		t.Errorf("USER_KEY: got %q want %q", got["USER_KEY"], "user-value")
	}
	if got["REPO_KEY"] != "repo-value" {
		t.Errorf("REPO_KEY: got %q want %q", got["REPO_KEY"], "repo-value")
	}
	// repo shadows user — same precedence as the existing repo-shadows-
	// org rule.
	if got["SHARED"] != "repo-shared" {
		t.Errorf("SHARED: got %q (user-secret leaked through?), want repo-shared", got["SHARED"])
	}
}

// TestResolveVisibleSecrets_EnforceFiltersUserScope confirms that when
// the UserActionsSecrets enforce flag is set AND the owner doesn't
// have the entitlement, user-scope rows are dropped from the resolved
// map. The repo-scope rows still flow through.
func TestResolveVisibleSecrets_EnforceFiltersUserScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	box := mustBox(t)

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		BillingEnforce: config.EnforceConfig{
			UserActionsSecrets: true,
		},
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserSecret(t, pool, box, userID, "PERSONAL", []byte("u"))
	mustRepoSecret(t, pool, box, repoID, "REPO_ONLY", []byte("r"))

	got, err := h.resolveVisibleSecretsFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := got["PERSONAL"]; ok {
		t.Errorf("user-scope row should be filtered for non-Pro under enforce; got=%+v", got)
	}
	if got["REPO_ONLY"] != "r" {
		t.Errorf("repo-scope row should still flow; got=%+v", got)
	}
}

// TestResolveVisibleSecrets_RunnerEmitsReportOnlyDeny pins that the
// runner-side gate emits `entitlements.report_only_deny` whenever the
// owner lacks the entitlement — regardless of whether the enforce
// flag is set. Without this log line, an SRE flipping the enforce
// flag in PRO-EXT01-17 has no soak-window evidence to consult. The
// emission is the contract guarded by PRO-EXT_SR-02.
func TestResolveVisibleSecrets_RunnerEmitsReportOnlyDeny(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	box := mustBox(t)
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    logger,
		// Enforce flag intentionally off: the log line must fire
		// during the soak window too.
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserSecret(t, pool, box, userID, "PERSONAL", []byte("u"))

	if _, err := h.resolveVisibleSecretsFromDB(context.Background(), pool, repoID, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	out := logBuf.String()
	if !strings.Contains(out, `"msg":"entitlements.report_only_deny"`) {
		t.Errorf("expected report_only_deny log line; got=%s", out)
	}
	if !strings.Contains(out, `"feature":"user_actions_secrets"`) {
		t.Errorf("log line should name the feature: %s", out)
	}
	if !strings.Contains(out, `"surface":"runner"`) {
		t.Errorf("log line should tag the runner surface: %s", out)
	}
}

// TestResolveVisibleSecrets_ProUserMergesUnderEnforce pins the
// positive case the audit flagged as missing: a Pro user owns the
// repo, enforce is ON, and user-scope rows MUST still flow through.
// Without this test, a refactor that denied all principals
// (regardless of plan) would silently break Pro users' workflows
// and only get caught in production.
func TestResolveVisibleSecrets_ProUserMergesUnderEnforce(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	box := mustBox(t)

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		BillingEnforce: config.EnforceConfig{
			UserActionsSecrets: true,
		},
	}}

	userID := mustUser(t, pool, "alice")
	mustUpgradeUserToPro(t, pool, userID)
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserSecret(t, pool, box, userID, "PRO_KEY", []byte("pro-value"))
	mustRepoSecret(t, pool, box, repoID, "REPO_KEY", []byte("repo-value"))

	got, err := h.resolveVisibleSecretsFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["PRO_KEY"] != "pro-value" {
		t.Errorf("Pro user user-scope row should merge under enforce; got %q", got["PRO_KEY"])
	}
	if got["REPO_KEY"] != "repo-value" {
		t.Errorf("repo-scope row should always merge; got %q", got["REPO_KEY"])
	}
}

// TestResolveVisibleSecrets_FreeUserReportOnlyMergesUserScope pins
// the second missing-cell from the audit: Free user, enforce OFF
// (the default soak-window state). The gate is report-only, so the
// merge must complete — the soak's whole point is observing what
// WOULD have been denied while still serving the user's intent.
func TestResolveVisibleSecrets_FreeUserReportOnlyMergesUserScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	box := mustBox(t)

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Enforce flag OFF — the campaign's default soak-window state.
	}}

	userID := mustUser(t, pool, "alice")
	// No upgradeUserToPro — alice stays Free.
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserSecret(t, pool, box, userID, "REPORT_ONLY_KEY", []byte("ro-value"))
	mustRepoSecret(t, pool, box, repoID, "REPO_KEY", []byte("repo-value"))

	got, err := h.resolveVisibleSecretsFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["REPORT_ONLY_KEY"] != "ro-value" {
		t.Errorf("report-only must honor user-scope rows; got %q", got["REPORT_ONLY_KEY"])
	}
	if got["REPO_KEY"] != "repo-value" {
		t.Errorf("repo-scope row should always merge; got %q", got["REPO_KEY"])
	}
}

func TestResolveVisibleSecrets_EnvironmentScopeShadowsRepo(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	box := mustBox(t)

	h := &Handlers{d: Deps{
		Pool:      pool,
		SecretBox: box,
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")
	envID := mustRepoEnvironment(t, pool, repoID, "production")
	mustRepoSecret(t, pool, box, repoID, "SHARED", []byte("repo-value"))
	mustEnvironmentSecret(t, pool, box, envID, "SHARED", []byte("env-value"))
	mustEnvironmentSecret(t, pool, box, envID, "DEPLOY_TOKEN", []byte("deploy-value"))

	got, err := h.resolveVisibleSecretsForJobFromDB(context.Background(), pool, repoID, "", "production")
	if err != nil {
		t.Fatalf("resolve production: %v", err)
	}
	if got["SHARED"] != "env-value" {
		t.Fatalf("environment secret should shadow repo secret; got %+v", got)
	}
	if got["DEPLOY_TOKEN"] != "deploy-value" {
		t.Fatalf("environment-only secret missing; got %+v", got)
	}

	withoutEnv, err := h.resolveVisibleSecretsForJobFromDB(context.Background(), pool, repoID, "", "staging")
	if err != nil {
		t.Fatalf("resolve staging: %v", err)
	}
	if withoutEnv["SHARED"] != "repo-value" {
		t.Fatalf("unconfigured environment should not merge env secrets; got %+v", withoutEnv)
	}

	prSecrets, err := h.resolveVisibleSecretsForJobFromDB(context.Background(), pool, repoID, actionsdb.WorkflowRunEventPullRequest, "production")
	if err != nil {
		t.Fatalf("resolve pull_request: %v", err)
	}
	if len(prSecrets) != 0 {
		t.Fatalf("pull_request run must not receive environment secrets; got %+v", prSecrets)
	}
}

// ─── helpers ────────────────────────────────────────────────────────

// Helpers take testing.TB so the benchmark file can reuse them
// without forcing a sub-test wrapper that muddies per-iteration
// accounting (PRO-EXT_SR-08 added BenchmarkResolveVisibleSecrets_*).
func mustBox(t testing.TB) *secretbox.Box {
	t.Helper()
	k, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("secretbox key: %v", err)
	}
	b, err := secretbox.FromBytes(k)
	if err != nil {
		t.Fatalf("secretbox FromBytes: %v", err)
	}
	return b
}

func mustUser(t testing.TB, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (username, password_hash, session_epoch)
		VALUES ($1, '!', 1) RETURNING id
	`, username).Scan(&id); err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	return id
}

func mustRepo(t testing.TB, pool *pgxpool.Pool, ownerUserID int64, name string) int64 {
	t.Helper()
	row, err := reposdb.New().CreateRepo(context.Background(), pool, reposdb.CreateRepoParams{
		Name:          name,
		OwnerUserID:   pgtype.Int8{Int64: ownerUserID, Valid: true},
		Visibility:    reposdb.RepoVisibilityPrivate,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return row.ID
}

func mustUserSecret(t testing.TB, pool *pgxpool.Pool, box *secretbox.Box, userID int64, name string, plaintext []byte) {
	t.Helper()
	ct, nonce, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := actionsdb.New().UpsertUserSecret(context.Background(), pool, actionsdb.UpsertUserSecretParams{
		UserID:     pgtype.Int8{Int64: userID, Valid: true},
		Name:       name,
		Ciphertext: ct,
		Nonce:      nonce,
	}); err != nil {
		t.Fatalf("UpsertUserSecret: %v", err)
	}
}

func mustRepoSecret(t testing.TB, pool *pgxpool.Pool, box *secretbox.Box, repoID int64, name string, plaintext []byte) {
	t.Helper()
	ct, nonce, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := actionsdb.New().UpsertRepoSecret(context.Background(), pool, actionsdb.UpsertRepoSecretParams{
		RepoID:     pgtype.Int8{Int64: repoID, Valid: true},
		Name:       name,
		Ciphertext: ct,
		Nonce:      nonce,
	}); err != nil {
		t.Fatalf("UpsertRepoSecret: %v", err)
	}
}

func mustRepoEnvironment(t testing.TB, pool *pgxpool.Pool, repoID int64, name string) int64 {
	t.Helper()
	env, err := actionsdb.New().UpsertRepoEnvironment(context.Background(), pool, actionsdb.UpsertRepoEnvironmentParams{
		RepoID:                   repoID,
		Name:                     name,
		RequiredReviewersEnabled: false,
		PreventSelfReview:        false,
		WaitTimerMinutes:         0,
		DeploymentBranchPolicy:   actionsdb.RepoEnvironmentDeploymentBranchPolicyAll,
	})
	if err != nil {
		t.Fatalf("UpsertRepoEnvironment: %v", err)
	}
	return env.ID
}

func mustEnvironmentSecret(t testing.TB, pool *pgxpool.Pool, box *secretbox.Box, environmentID int64, name string, plaintext []byte) {
	t.Helper()
	ct, nonce, err := box.Seal(plaintext)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := actionsdb.New().UpsertEnvironmentSecret(context.Background(), pool, actionsdb.UpsertEnvironmentSecretParams{
		EnvironmentID: pgtype.Int8{Int64: environmentID, Valid: true},
		Name:          name,
		Ciphertext:    ct,
		Nonce:         nonce,
	}); err != nil {
		t.Fatalf("UpsertEnvironmentSecret: %v", err)
	}
}

// mustUpgradeUserToPro promotes the test user to an active Pro
// subscription via the canonical entitlements snapshot path. Mirrors
// the api_test-package helper (upgradeUserToActivePro in
// user_plan_test.go) but is reachable from this in-package file.
func mustUpgradeUserToPro(t *testing.T, pool *pgxpool.Pool, userID int64) {
	t.Helper()
	now := time.Now().UTC()
	suffix := strconv.FormatInt(userID, 10)
	if _, err := billingdb.New().ApplyUserSubscriptionSnapshot(context.Background(), pool, billingdb.ApplyUserSubscriptionSnapshotParams{
		UserID:               userID,
		Plan:                 billingdb.UserPlanPro,
		SubscriptionStatus:   billingdb.BillingSubscriptionStatusActive,
		StripeSubscriptionID: pgtype.Text{String: "sub_runner_pro_" + suffix, Valid: true},
		CurrentPeriodStart:   pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		CurrentPeriodEnd:     pgtype.Timestamptz{Time: now.Add(30 * 24 * time.Hour), Valid: true},
		LastWebhookEventID:   "evt_runner_pro_" + suffix,
	}); err != nil {
		t.Fatalf("ApplyUserSubscriptionSnapshot: %v", err)
	}
}
