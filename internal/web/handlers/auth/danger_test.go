// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func loginDangerUser(t *testing.T, name string) (cli *client, pool *pgxpool.Pool, captor *captureSender) {
	t.Helper()
	httpsrv, pool, captor := newTestServerWithPool(t, false)
	cli = newClient(t, httpsrv)
	mustSignup(t, cli, name, name+"@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {name},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli, pool, captor
}

func TestDanger_DeleteRoundtripAndRestore(t *testing.T) {
	t.Parallel()
	cli, _, _ := loginDangerUser(t, "danga")

	// Wrong username should be rejected.
	csrf := cli.extractCSRF(t, "/settings/danger")
	resp := cli.post(t, "/settings/danger", url.Values{
		"csrf_token":       {csrf},
		"confirm_username": {"not-me"},
		"password":         {"correct horse battery staple"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Type your username") {
		t.Fatalf("expected confirm-username error, got: %s", body)
	}

	// Wrong password should be rejected.
	csrf = cli.extractCSRF(t, "/settings/danger")
	resp = cli.post(t, "/settings/danger", url.Values{
		"csrf_token":       {csrf},
		"confirm_username": {"danga"},
		"password":         {"definitely-not-the-password"},
	})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Password is incorrect") {
		t.Fatalf("expected password error, got: %s", body)
	}

	// Correct credentials -> 303 to "/?notice=account-deleted".
	csrf = cli.extractCSRF(t, "/settings/danger")
	resp = cli.post(t, "/settings/danger", url.Values{
		"csrf_token":       {csrf},
		"confirm_username": {"danga"},
		"password":         {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete: status=%d body=%s", resp.StatusCode, body)
	}
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "account-deleted") {
		t.Fatalf("Location=%q", loc)
	}
	_ = resp.Body.Close()

	// /settings/profile is now unreachable for this client (epoch stale +
	// cookie cleared). RequireUser bounces to /login.
	resp = cli.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("post-delete /settings/profile expected redirect, got %d", resp.StatusCode)
	}

	// Restore-on-login: signing in again with the SAME credentials
	// should clear deleted_at. The login attempt itself returns the
	// usual 303 to /.
	cli2 := newClient(t, cli.srv)
	csrf = cli2.extractCSRF(t, "/login")
	resp = cli2.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"danga"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("restore-login: status=%d body=%s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Now /settings/profile is back.
	resp = cli2.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post-restore /settings/profile expected 200, got %d", resp.StatusCode)
	}
}

func TestDanger_PostGracePermanent(t *testing.T) {
	t.Parallel()
	cli, pool, _ := loginDangerUser(t, "dangb")

	// Delete normally.
	csrf := cli.extractCSRF(t, "/settings/danger")
	resp := cli.post(t, "/settings/danger", url.Values{
		"csrf_token":       {csrf},
		"confirm_username": {"dangb"},
		"password":         {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Backdate deleted_at past the grace window so the next login should
	// be treated as nonexistent. Uses the SAME pool the test server is
	// reading from — a fresh dbtest.NewTestDB call would clone a brand
	// new database.
	if _, err := pool.Exec(
		context.Background(),
		"UPDATE users SET deleted_at = $1 WHERE username = 'dangb'",
		time.Now().Add(-30*24*time.Hour),
	); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	cli2 := newClient(t, cli.srv)
	csrf = cli2.extractCSRF(t, "/login")
	resp = cli2.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"dangb"},
		"password":   {"correct horse battery staple"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Incorrect username or password") {
		t.Fatalf("expected post-grace login to be treated as wrong, got: %s", body)
	}
}
