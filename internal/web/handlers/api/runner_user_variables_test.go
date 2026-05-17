// SPDX-License-Identifier: AGPL-3.0-or-later

package api

// PRO-EXT01-12c: mirrors runner_user_secrets_test.go for the
// variables resolution path. The shape of the gate is identical
// (entitlement check, report-only soak, enforce-flag merge filter)
// but the wire format is plaintext — no SecretBox. Test independently
// so a refactor that drops one entitlement check can't silently land
// against the other.

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

func TestResolveVisibleVariables_MergesUserScopeForUserOwnedRepos(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	h := &Handlers{d: Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")

	mustUserVariable(t, pool, userID, "USER_VAR", "user-value")
	mustUserVariable(t, pool, userID, "SHARED", "user-shared")
	mustRepoVariable(t, pool, repoID, "REPO_VAR", "repo-value")
	mustRepoVariable(t, pool, repoID, "SHARED", "repo-shared")

	got, err := h.resolveVisibleVariablesFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["USER_VAR"] != "user-value" {
		t.Errorf("USER_VAR: got %q want %q", got["USER_VAR"], "user-value")
	}
	if got["REPO_VAR"] != "repo-value" {
		t.Errorf("REPO_VAR: got %q want %q", got["REPO_VAR"], "repo-value")
	}
	if got["SHARED"] != "repo-shared" {
		t.Errorf("SHARED: got %q (user-var leaked through?), want repo-shared", got["SHARED"])
	}
}

// TestResolveVisibleVariables_EnforceFiltersUserScope: same shape as
// the secrets equivalent. Free + enforce-on → user-scope vars
// dropped; repo-scope still flows.
func TestResolveVisibleVariables_EnforceFiltersUserScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	h := &Handlers{d: Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		BillingEnforce: config.EnforceConfig{
			UserActionsVariables: true,
		},
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserVariable(t, pool, userID, "PERSONAL", "u")
	mustRepoVariable(t, pool, repoID, "REPO_ONLY", "r")

	got, err := h.resolveVisibleVariablesFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := got["PERSONAL"]; ok {
		t.Errorf("user-scope var should be filtered for non-Pro under enforce; got=%+v", got)
	}
	if got["REPO_ONLY"] != "r" {
		t.Errorf("repo-scope var should still flow; got=%+v", got)
	}
}

// TestResolveVisibleVariables_RunnerEmitsReportOnlyDeny pins the
// telemetry contract: the variables-side gate emits a deny log with
// surface=runner-vars so SREs can split the soak signal from the
// secrets one. Without this, the operator dashboard for sprint 17's
// enforce flip would conflate the two features' deny rates.
func TestResolveVisibleVariables_RunnerEmitsReportOnlyDeny(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	h := &Handlers{d: Deps{
		Pool:   pool,
		Logger: logger,
	}}

	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserVariable(t, pool, userID, "PERSONAL", "u")

	if _, err := h.resolveVisibleVariablesFromDB(context.Background(), pool, repoID, ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	out := logBuf.String()
	if !strings.Contains(out, `"msg":"entitlements.report_only_deny"`) {
		t.Errorf("expected report_only_deny log line; got=%s", out)
	}
	if !strings.Contains(out, `"feature":"user_actions_variables"`) {
		t.Errorf("log line should name the variables feature: %s", out)
	}
	if !strings.Contains(out, `"surface":"runner-vars"`) {
		t.Errorf("log line should tag runner-vars surface to distinguish from secrets: %s", out)
	}
}

// TestResolveVisibleVariables_ProUserMergesUnderEnforce confirms the
// positive case: Pro user, enforce ON, user-scope vars flow through.
func TestResolveVisibleVariables_ProUserMergesUnderEnforce(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	h := &Handlers{d: Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		BillingEnforce: config.EnforceConfig{
			UserActionsVariables: true,
		},
	}}

	userID := mustUser(t, pool, "alice")
	mustUpgradeUserToPro(t, pool, userID)
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserVariable(t, pool, userID, "PRO_VAR", "pro-value")
	mustRepoVariable(t, pool, repoID, "REPO_VAR", "repo-value")

	got, err := h.resolveVisibleVariablesFromDB(context.Background(), pool, repoID, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got["PRO_VAR"] != "pro-value" {
		t.Errorf("Pro user user-scope var should merge under enforce; got %q", got["PRO_VAR"])
	}
	if got["REPO_VAR"] != "repo-value" {
		t.Errorf("repo-scope var should always merge; got %q", got["REPO_VAR"])
	}
}

// TestResolveVisibleVariables_PullRequestEventEmpty pins the security
// boundary the secrets path enforces too: PR-from-fork events get
// no vars. Otherwise a malicious PR could exfiltrate user-scope vars
// via a workflow run on the maintainer's repo.
func TestResolveVisibleVariables_PullRequestEventEmpty(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	h := &Handlers{d: Deps{
		Pool:   pool,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}
	userID := mustUser(t, pool, "alice")
	repoID := mustRepo(t, pool, userID, "demo")
	mustUserVariable(t, pool, userID, "SECRETISH", "leaky")
	mustRepoVariable(t, pool, repoID, "REPO_VAR", "v")

	got, err := h.resolveVisibleVariablesFromDB(context.Background(), pool, repoID, actionsdb.WorkflowRunEventPullRequest)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Errorf("PR event must return nil vars; got %+v", got)
	}
}

// ─── helpers ──────────────────────────────────────────────────────

func mustUserVariable(t testing.TB, pool *pgxpool.Pool, userID int64, name, value string) {
	t.Helper()
	if _, err := actionsdb.New().UpsertUserVariable(context.Background(), pool, actionsdb.UpsertUserVariableParams{
		UserID:          pgtype.Int8{Int64: userID, Valid: true},
		Name:            name,
		Value:           value,
		CreatedByUserID: pgtype.Int8{Int64: userID, Valid: true},
	}); err != nil {
		t.Fatalf("UpsertUserVariable: %v", err)
	}
}

func mustRepoVariable(t testing.TB, pool *pgxpool.Pool, repoID int64, name, value string) {
	t.Helper()
	if _, err := actionsdb.New().UpsertRepoVariable(context.Background(), pool, actionsdb.UpsertRepoVariableParams{
		RepoID:          pgtype.Int8{Int64: repoID, Valid: true},
		Name:            name,
		Value:           value,
		CreatedByUserID: pgtype.Int8{Int64: repoID, Valid: true}, // dummy actor — handler-side validation lives elsewhere
	}); err != nil {
		t.Fatalf("UpsertRepoVariable: %v", err)
	}
}
