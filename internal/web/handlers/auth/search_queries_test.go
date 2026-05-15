// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PRO-EXT01-08a — settings → saved search queries CRUD + Pro gate.

// TestSavedSearchQueries_FreeReportOnlyLetsInsertLand pins the default
// report-only behaviour: a Free user POSTing the form succeeds; the
// would-deny is logged but the row lands.
func TestSavedSearchQueries_FreeReportOnlyLetsInsertLand(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freealice")

	csrf := cli.extractCSRF(t, "/settings/search-queries")
	resp := cli.post(t, "/settings/search-queries", url.Values{
		"csrf_token":   {csrf},
		"name":         {"open-todos"},
		"query_text":   {"TODO"},
		"scope_filter": {"alice"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Saved query added") {
		t.Fatalf("Free report-only insert should succeed: %s", body)
	}
	assertSearchQueriesCount(t, pool, userID, 1)
}

// TestSavedSearchQueries_EnforceBlocksFree pins the enforce path: a
// Free user's POST is denied with the upgrade banner.
func TestSavedSearchQueries_EnforceBlocksFree(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceAdvancedCodeSearch: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freebob")

	csrf := cli.extractCSRF(t, "/settings/search-queries")
	resp := cli.post(t, "/settings/search-queries", url.Values{
		"csrf_token": {csrf},
		"name":       {"open-todos"},
		"query_text": {"TODO"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Upgrade") {
		t.Fatalf("enforce should surface upgrade copy: %s", body)
	}
	assertSearchQueriesCount(t, pool, userID, 0)
}

// TestSavedSearchQueries_ProAlwaysAllowed verifies a Pro user POSTs
// successfully even with enforce on.
func TestSavedSearchQueries_ProAlwaysAllowed(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceAdvancedCodeSearch: true,
	})
	cli, userID := newBillingTestUser(t, srv, pool, captor, "procarol")
	upgradeProfileTestUserToPro(t, pool, userID)

	csrf := cli.extractCSRF(t, "/settings/search-queries")
	resp := cli.post(t, "/settings/search-queries", url.Values{
		"csrf_token": {csrf},
		"name":       {"my-perf-queries"},
		"query_text": {"performance"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Saved query added") {
		t.Fatalf("Pro should be accepted under enforce: %s", body)
	}
	assertSearchQueriesCount(t, pool, userID, 1)
}

// TestSavedSearchQueries_AllowedSignalReflectsEntitlement confirms
// the GET page surfaces ALLOWED=true for Pro and ALLOWED=false for
// Free, driving the disabled-form locked UI for Free users.
func TestSavedSearchQueries_AllowedSignalReflectsEntitlement(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freedan")

	resp := cli.get(t, "/settings/search-queries")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ALLOWED=false") {
		t.Fatalf("Free user should see ALLOWED=false: %s", body)
	}

	upgradeProfileTestUserToPro(t, pool, userID)
	resp = cli.get(t, "/settings/search-queries")
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "ALLOWED=true") {
		t.Fatalf("Pro user should see ALLOWED=true: %s", body)
	}
}

// TestSavedSearchQueries_UpdateAndDelete rounds out CRUD.
func TestSavedSearchQueries_UpdateAndDelete(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, userID := newBillingTestUser(t, srv, pool, captor, "freeed")

	csrf := cli.extractCSRF(t, "/settings/search-queries")
	cli.post(t, "/settings/search-queries", url.Values{
		"csrf_token": {csrf},
		"name":       {"first"},
		"query_text": {"hello"},
	}).Body.Close()

	var id int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT id FROM user_code_search_saved_queries WHERE user_id = $1 AND name = $2`,
		userID, "first",
	).Scan(&id); err != nil {
		t.Fatalf("lookup id: %v", err)
	}

	csrf = cli.extractCSRF(t, "/settings/search-queries")
	resp := cli.post(t, "/settings/search-queries/"+strconv.FormatInt(id, 10)+"/update", url.Values{
		"csrf_token":   {csrf},
		"name":         {"first-renamed"},
		"query_text":   {"goodbye"},
		"scope_filter": {"alice/repo"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "updated") {
		t.Fatalf("update should succeed: %s", body)
	}

	csrf = cli.extractCSRF(t, "/settings/search-queries")
	resp = cli.post(t, "/settings/search-queries/"+strconv.FormatInt(id, 10)+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "deleted") {
		t.Fatalf("delete should succeed: %s", body)
	}
	assertSearchQueriesCount(t, pool, userID, 0)
}

// TestSavedSearchQueries_DuplicateNameRejected exercises the
// (user_id, name) UNIQUE constraint via the friendly handler-side
// message; citext means case-insensitive collision.
func TestSavedSearchQueries_DuplicateNameRejected(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPool(t, false)
	cli, _ := newBillingTestUser(t, srv, pool, captor, "freedup")

	csrf := cli.extractCSRF(t, "/settings/search-queries")
	cli.post(t, "/settings/search-queries", url.Values{
		"csrf_token": {csrf},
		"name":       {"perf"},
		"query_text": {"performance"},
	}).Body.Close()

	csrf = cli.extractCSRF(t, "/settings/search-queries")
	resp := cli.post(t, "/settings/search-queries", url.Values{
		"csrf_token": {csrf},
		"name":       {"PERF"},
		"query_text": {"perf"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "already have a saved query") {
		t.Fatalf("citext duplicate should be rejected: %s", body)
	}
}

func assertSearchQueriesCount(t *testing.T, pool *pgxpool.Pool, userID int64, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM user_code_search_saved_queries WHERE user_id = $1`, userID,
	).Scan(&got); err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != want {
		t.Errorf("count for %d: got %d, want %d", userID, got, want)
	}
}
