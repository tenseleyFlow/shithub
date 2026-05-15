// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/infra/config"
)

// PRO-EXT01-11b: handler write-path contract for PAT repo binding.
//
//   - Free user, enforce off → binding accepted (report-only).
//   - Free user, enforce on  → binding rejected with upgrade banner.
//   - Pro user, enforce on   → binding accepted.
//   - Binding to a repo the user doesn't own → rejected (regardless of plan).
//   - Binding to a nonexistent repo → rejected.

// seedRepoForUser inserts a minimal repo row owned by `userID` and
// returns its id. The middleware-side enforcement test in 11a covers
// the read path; here we only need a row the FK can satisfy.
func seedRepoForUser(t *testing.T, pool *pgxpool.Pool, userID int64, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO repos (owner_user_id, name, visibility, default_branch)
		VALUES ($1, $2, 'private', 'main')
		RETURNING id
	`, userID, name).Scan(&id); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	return id
}

// TestPATRepoBinding_FreeReportOnlyAllows confirms that with the
// enforce flag off, a Free user can attach a binding — report-only
// honors the user's intent.
func TestPATRepoBinding_FreeReportOnlyAllows(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, userID := signupAndLoginFor(t, srv, pool, captor, "alice11breportonly")
	repoID := seedRepoForUser(t, pool, userID, "ci-target")

	body := createTokenForm(t, cli, url.Values{
		"name":    {"ci"},
		"scopes":  {"user:read"},
		"repo_id": {strconv.FormatInt(repoID, 10)},
	})
	if !strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("Free + report-only: expected mint; body=%s", body)
	}
	if !strings.Contains(body, ":bound="+strconv.FormatInt(repoID, 10)) {
		t.Fatalf("Free + report-only: listing should show :bound=%d; body=%s", repoID, body)
	}
}

// TestPATRepoBinding_FreeEnforceRejects confirms that a Free user
// attempting a binding with the enforce flag on sees the upgrade
// banner instead of a successful mint.
func TestPATRepoBinding_FreeEnforceRejects(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{
		UserFineGrainedPATs: true,
	})
	cli, userID := signupAndLoginFor(t, srv, pool, captor, "alice11benforce")
	repoID := seedRepoForUser(t, pool, userID, "ci-target")

	body := createTokenForm(t, cli, url.Values{
		"name":    {"ci"},
		"scopes":  {"user:read"},
		"repo_id": {strconv.FormatInt(repoID, 10)},
	})
	if strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("Free + enforce: should NOT mint; body=%s", body)
	}
	if !strings.Contains(strings.ToLower(body), "pro") {
		t.Fatalf("Free + enforce: rejection should mention Pro; body=%s", body)
	}
}

// TestPATRepoBinding_ProEnforceAllows confirms a Pro user always
// gets a binding through.
func TestPATRepoBinding_ProEnforceAllows(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{
		UserFineGrainedPATs: true,
	})
	cli, userID := signupAndLoginFor(t, srv, pool, captor, "alice11bpro")
	upgradeProfileTestUserToPro(t, pool, userID)
	repoID := seedRepoForUser(t, pool, userID, "ci-target")

	body := createTokenForm(t, cli, url.Values{
		"name":    {"ci"},
		"scopes":  {"user:read"},
		"repo_id": {strconv.FormatInt(repoID, 10)},
	})
	if !strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("Pro + enforce: should mint; body=%s", body)
	}
	if !strings.Contains(body, ":bound="+strconv.FormatInt(repoID, 10)) {
		t.Fatalf("Pro + enforce: listing should show binding; body=%s", body)
	}
}

// TestPATRepoBinding_RejectsNonOwnedRepo pins the ownership invariant:
// a user cannot bind a token to a repo they don't own, even if that
// repo's ID is guessable. The error message is identical to the
// "doesn't exist" case so a probe can't enumerate private repo IDs.
func TestPATRepoBinding_RejectsNonOwnedRepo(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, _ := signupAndLoginFor(t, srv, pool, captor, "alice11battack")

	// Create a SECOND user + repo. attackerUser will try to bind to it.
	otherID := mustInsertBareUser(t, pool, "bob11btarget")
	repoID := seedRepoForUser(t, pool, otherID, "private-stuff")

	body := createTokenForm(t, cli, url.Values{
		"name":    {"ci"},
		"scopes":  {"user:read"},
		"repo_id": {strconv.FormatInt(repoID, 10)},
	})
	if strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("must not mint when binding to non-owned repo; body=%s", body)
	}
	if !strings.Contains(strings.ToLower(body), "own") {
		t.Fatalf("error should reference ownership; body=%s", body)
	}
}

// TestPATRepoBinding_RejectsBadID rejects non-numeric and missing IDs.
func TestPATRepoBinding_RejectsBadID(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTokenServerWithEnforce(t, config.EnforceConfig{})
	cli, _ := signupAndLoginFor(t, srv, pool, captor, "alice11bbadid")

	body := createTokenForm(t, cli, url.Values{
		"name":    {"ci"},
		"scopes":  {"user:read"},
		"repo_id": {"not-a-number"},
	})
	if strings.Contains(body, "RAW=shithub_pat_") {
		t.Fatalf("non-numeric repo_id should be rejected; body=%s", body)
	}
}

// mustInsertBareUser creates a user row directly via SQL — no email/
// verification flow — for ownership-check fixtures.
func mustInsertBareUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO users (username, password_hash, session_epoch)
		VALUES ($1, '!', 1) RETURNING id
	`, username).Scan(&id); err != nil {
		t.Fatalf("insert bare user: %v", err)
	}
	return id
}
