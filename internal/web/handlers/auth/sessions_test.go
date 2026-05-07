// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func loginSessionsUser(t *testing.T, name string) (cli *client, loginAgain func() *client) {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli = newClient(t, httpsrv)
	mustSignup(t, cli, name, name+"@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	doLogin := func(c *client) {
		csrf := c.extractCSRF(t, "/login")
		resp := c.post(t, "/login", url.Values{
			"csrf_token": {csrf},
			"username":   {name},
			"password":   {"correct horse battery staple"},
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("login: %d", resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	doLogin(cli)
	// loginAgain returns a fresh client that has signed in as the same
	// user — useful for testing log-out-everywhere across two browsers.
	loginAgain = func() *client {
		c := newClient(t, httpsrv)
		doLogin(c)
		return c
	}
	return cli, loginAgain
}

func TestSessions_PageRenders(t *testing.T) {
	t.Parallel()
	cli, _ := loginSessionsUser(t, "sa")
	resp := cli.get(t, "/settings/sessions")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Sessions") {
		t.Fatalf("missing heading: %s", body)
	}
	if !strings.Contains(string(body), "UA=Go-http-client") {
		t.Fatalf("expected User-Agent surfaced; got: %s", body)
	}
}

func TestSessions_LogoutEverywhereInvalidatesOthers(t *testing.T) {
	t.Parallel()
	cliA, loginAgain := loginSessionsUser(t, "sb")
	cliB := loginAgain() // a second "browser"

	// Both browsers can hit the protected page initially.
	resp := cliB.get(t, "/settings/profile")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cliB before bump: status=%d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// cliA bumps the epoch.
	csrf := cliA.extractCSRF(t, "/settings/sessions")
	resp = cliA.post(t, "/settings/sessions/logout-everywhere", url.Values{
		"csrf_token": {csrf},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Signed out of every other session") {
		t.Fatalf("expected success message, got: %s", body)
	}

	// cliA itself stays signed in.
	resp = cliA.get(t, "/settings/profile")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("cliA after bump should still be signed in: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// cliB should now be bounced to /login on the next protected hit
	// because its session carries the stale epoch.
	resp = cliB.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("cliB after bump expected redirect, got %d", resp.StatusCode)
	}
}
