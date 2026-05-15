// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// PRO-EXT01-09 — contribution privacy CRUD + Pro gate.

// TestContributionPrivacy_AllowedSignalReflectsEntitlement asserts the
// rendered page surfaces ALLOWED=false for Free and ALLOWED=true for
// Pro — the signal that drives the disabled-checkbox locked UI.
func TestContributionPrivacy_AllowedSignalReflectsEntitlement(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freealice")

	resp := cli.get(t, "/settings/contributions")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ALLOWED=false") {
		t.Fatalf("Free user should see ALLOWED=false: %s", body)
	}

	upgradeProfileTestUserToPro(t, pool, userID)
	resp = cli.get(t, "/settings/contributions")
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ALLOWED=true") {
		t.Fatalf("Pro user should see ALLOWED=true: %s", body)
	}
}

// TestContributionPrivacy_FreeReportOnlyLetsOptoutLand pins the
// default report-only behaviour: a Free user's submit succeeds.
func TestContributionPrivacy_FreeReportOnlyLetsOptoutLand(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freebob")
	repoID := seedOwnedRepo(t, pool, userID, "side-project")

	csrf := cli.extractCSRF(t, "/settings/contributions")
	resp := cli.post(t, "/settings/contributions", url.Values{
		"csrf_token":     {csrf},
		"optout_repo_id": {strconv.FormatInt(repoID, 10)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "updated") {
		t.Fatalf("Free report-only insert should succeed: %s", body)
	}
	assertOptoutCount(t, pool, userID, 1)
}

// TestContributionPrivacy_EnforceBlocksFree pins the enforce path.
func TestContributionPrivacy_EnforceBlocksFree(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceContributionPrivacy: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freecarl")
	repoID := seedOwnedRepo(t, pool, userID, "side-project")

	csrf := cli.extractCSRF(t, "/settings/contributions")
	resp := cli.post(t, "/settings/contributions", url.Values{
		"csrf_token":     {csrf},
		"optout_repo_id": {strconv.FormatInt(repoID, 10)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Upgrade") {
		t.Fatalf("enforce should surface upgrade copy: %s", body)
	}
	assertOptoutCount(t, pool, userID, 0)
}

// TestContributionPrivacy_ProAlwaysAllowed verifies a Pro user submits
// successfully even with enforce on.
func TestContributionPrivacy_ProAlwaysAllowed(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceContributionPrivacy: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "prodan")
	upgradeProfileTestUserToPro(t, pool, userID)
	repoID := seedOwnedRepo(t, pool, userID, "side-project")

	csrf := cli.extractCSRF(t, "/settings/contributions")
	resp := cli.post(t, "/settings/contributions", url.Values{
		"csrf_token":     {csrf},
		"optout_repo_id": {strconv.FormatInt(repoID, 10)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "updated") {
		t.Fatalf("Pro accept under enforce should succeed: %s", body)
	}
	assertOptoutCount(t, pool, userID, 1)
}

// TestContributionPrivacy_ReconcileDeletesUnchecked verifies that
// re-submitting the form without a previously-checked id removes
// that opt-out row.
func TestContributionPrivacy_ReconcileDeletesUnchecked(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freeed")
	repoID := seedOwnedRepo(t, pool, userID, "first")

	csrf := cli.extractCSRF(t, "/settings/contributions")
	cli.post(t, "/settings/contributions", url.Values{
		"csrf_token":     {csrf},
		"optout_repo_id": {strconv.FormatInt(repoID, 10)},
	}).Body.Close()
	assertOptoutCount(t, pool, userID, 1)

	// Submit with no opt-out checkboxes → existing row removed.
	csrf = cli.extractCSRF(t, "/settings/contributions")
	cli.post(t, "/settings/contributions", url.Values{
		"csrf_token": {csrf},
	}).Body.Close()
	assertOptoutCount(t, pool, userID, 0)
}

// TestContributionPrivacy_RejectsForeignRepo verifies the defense-in-
// depth check: a submitted repo id that doesn't belong to the actor
// is rejected without inserting anything.
func TestContributionPrivacy_RejectsForeignRepo(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cliA, userA := newBillingTestUser(t, srv, pool, captor, "alicep1")
	_, userB := newBillingTestUser(t, srv, pool, captor, "bobp1")
	foreignRepoID := seedOwnedRepo(t, pool, userB, "bobs-repo")

	csrf := cliA.extractCSRF(t, "/settings/contributions")
	resp := cliA.post(t, "/settings/contributions", url.Values{
		"csrf_token":     {csrf},
		"optout_repo_id": {strconv.FormatInt(foreignRepoID, 10)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "not owned") {
		t.Fatalf("foreign repo opt-out should be rejected: %s", body)
	}
	assertOptoutCount(t, pool, userA, 0)
}

func seedOwnedRepo(t *testing.T, pool *pgxpool.Pool, userID int64, name string) int64 {
	t.Helper()
	row, err := reposdb.New().CreateRepo(context.Background(), pool, reposdb.CreateRepoParams{
		OwnerUserID:   pgtype.Int8{Int64: userID, Valid: true},
		Name:          name,
		DefaultBranch: "trunk",
		Visibility:    reposdb.RepoVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("CreateRepo %s: %v", name, err)
	}
	return row.ID
}

func assertOptoutCount(t *testing.T, pool *pgxpool.Pool, userID int64, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM user_contribution_repo_optouts WHERE user_id = $1`, userID,
	).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != want {
		t.Errorf("optout count for %d: got %d, want %d", userID, got, want)
	}
}
