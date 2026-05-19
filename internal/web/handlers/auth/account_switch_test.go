// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAccountSwitch_RemembersAuthenticatedAccountsInBrowser(t *testing.T) {
	t.Parallel()
	srv, pool, _ := newTestServerWithPool(t, false)
	cli := newClient(t, srv)

	const password = "correct horse battery staple"
	mustSignup(t, cli, "switchalice", "switchalice@example.com", password)
	mustSignup(t, cli, "switchbob", "switchbob@example.com", password)

	aliceID := lookupUserID(t, pool, "switchalice")
	bobID := lookupUserID(t, pool, "switchbob")

	loginAs(t, cli, "switchalice", password)
	assertProfileDisplay(t, cli, "switchalice")

	loginAs(t, cli, "switchbob", password)
	assertProfileDisplay(t, cli, "switchbob")

	switchAccount(t, cli, aliceID)
	assertProfileDisplay(t, cli, "switchalice")

	switchAccount(t, cli, bobID)
	assertProfileDisplay(t, cli, "switchbob")
}

func TestAccountSwitch_ForgetsStaleEpoch(t *testing.T) {
	t.Parallel()
	srv, pool, _ := newTestServerWithPool(t, false)
	cli := newClient(t, srv)

	const password = "correct horse battery staple"
	mustSignup(t, cli, "stalealice", "stalealice@example.com", password)
	mustSignup(t, cli, "stalebob", "stalebob@example.com", password)

	aliceID := lookupUserID(t, pool, "stalealice")

	loginAs(t, cli, "stalealice", password)
	loginAs(t, cli, "stalebob", password)

	if _, err := pool.Exec(context.Background(), "UPDATE users SET session_epoch = session_epoch + 1 WHERE id = $1", aliceID); err != nil {
		t.Fatalf("bump session_epoch: %v", err)
	}

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/account/switch", url.Values{
		"csrf_token": {csrf},
		"user_id":    {strconv.FormatInt(aliceID, 10)},
		"return_to":  {"/settings/profile"},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("stale switch: status %d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/login?notice=account-expired" {
		t.Fatalf("stale switch location: got %q", loc)
	}
	assertProfileDisplay(t, cli, "stalebob")
}

func loginAs(t *testing.T, cli *client, username, password string) {
	t.Helper()
	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {username},
		"password":   {password},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login %s: status %d body=%s", username, resp.StatusCode, body)
	}
}

func switchAccount(t *testing.T, cli *client, userID int64) {
	t.Helper()
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/account/switch", url.Values{
		"csrf_token": {csrf},
		"user_id":    {strconv.FormatInt(userID, 10)},
		"return_to":  {"/settings/profile"},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("switch account %d: status %d body=%s", userID, resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); loc != "/settings/profile" {
		t.Fatalf("switch account redirect: got %q", loc)
	}
}

func assertProfileDisplay(t *testing.T, cli *client, username string) {
	t.Helper()
	resp := cli.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("profile %s: status %d body=%s", username, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "DISPLAY="+username+";") {
		t.Fatalf("profile should be bound to %s, got body=%s", username, body)
	}
}

func lookupUserID(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), "SELECT id FROM users WHERE username = $1", username).Scan(&id); err != nil {
		t.Fatalf("lookup user %s: %v", username, err)
	}
	return id
}
