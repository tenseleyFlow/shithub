// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func loginAppearanceUser(t *testing.T, name string) *client {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)
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
	return cli
}

func TestAppearance_PersistsTheme(t *testing.T) {
	t.Parallel()
	cli := loginAppearanceUser(t, "appra")
	csrf := cli.extractCSRF(t, "/settings/appearance")
	resp := cli.post(t, "/settings/appearance", url.Values{
		"csrf_token": {csrf},
		"theme":      {"dark"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Appearance updated") {
		t.Fatalf("expected success, got: %s", body)
	}
	if !strings.Contains(string(body), "THEME=dark") {
		t.Fatalf("expected THEME=dark, got: %s", body)
	}

	// Cookie should also be set.
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == "theme" && c.Value == "dark" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected theme=dark cookie in response; got %+v", resp.Cookies())
	}
}

func TestAppearance_RejectsUnknownTheme(t *testing.T) {
	t.Parallel()
	cli := loginAppearanceUser(t, "apprb")
	csrf := cli.extractCSRF(t, "/settings/appearance")
	resp := cli.post(t, "/settings/appearance", url.Values{
		"csrf_token": {csrf},
		"theme":      {"neon"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Unknown theme") {
		t.Fatalf("expected unknown-theme error, got: %s", body)
	}
}

func TestAppearance_EmptyClearsCookie(t *testing.T) {
	t.Parallel()
	cli := loginAppearanceUser(t, "apprc")

	// Set a theme first.
	csrf := cli.extractCSRF(t, "/settings/appearance")
	_ = cli.post(t, "/settings/appearance", url.Values{
		"csrf_token": {csrf},
		"theme":      {"dark"},
	}).Body.Close()

	// Now clear it.
	csrf = cli.extractCSRF(t, "/settings/appearance")
	resp := cli.post(t, "/settings/appearance", url.Values{
		"csrf_token": {csrf},
		"theme":      {""},
	})
	defer func() { _ = resp.Body.Close() }()
	for _, c := range resp.Cookies() {
		if c.Name == "theme" && c.MaxAge >= 0 {
			t.Fatalf("expected theme cookie cleared (MaxAge<0); got %+v", c)
		}
	}
}
