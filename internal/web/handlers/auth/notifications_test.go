// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func loginNotifUser(t *testing.T, name string) *client {
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

func TestNotifications_DefaultsRender(t *testing.T) {
	t.Parallel()
	cli := loginNotifUser(t, "nfa")
	resp := cli.get(t, "/settings/notifications")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// Required is always on. account_changes default-on. product_news default-off.
	if !strings.Contains(string(body), "security_alerts:e=true:r=true") {
		t.Errorf("security_alerts row wrong: %s", body)
	}
	if !strings.Contains(string(body), "account_changes:e=true:r=false") {
		t.Errorf("account_changes default mismatch: %s", body)
	}
	if !strings.Contains(string(body), "product_news:e=false:r=false") {
		t.Errorf("product_news default mismatch: %s", body)
	}
}

func TestNotifications_OptOutThenOptIn(t *testing.T) {
	t.Parallel()
	cli := loginNotifUser(t, "nfb")

	// Opt out of account_changes (no checkbox sent for it).
	csrf := cli.extractCSRF(t, "/settings/notifications")
	resp := cli.post(t, "/settings/notifications", url.Values{
		"csrf_token": {csrf},
		// account_changes intentionally omitted
		"product_news": {"on"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Preferences saved") {
		t.Fatalf("expected save success, got: %s", body)
	}
	if !strings.Contains(string(body), "account_changes:e=false:r=false") {
		t.Errorf("account_changes should be off after opt-out: %s", body)
	}
	if !strings.Contains(string(body), "product_news:e=true:r=false") {
		t.Errorf("product_news should be on after opt-in: %s", body)
	}

	// Now flip back to defaults — DB rows for both should be deleted
	// since the desired state matches the default again.
	csrf = cli.extractCSRF(t, "/settings/notifications")
	resp = cli.post(t, "/settings/notifications", url.Values{
		"csrf_token":      {csrf},
		"account_changes": {"on"}, // back to default-on
		// product_news omitted → back to default-off
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "account_changes:e=true:r=false") {
		t.Errorf("expected account_changes back on: %s", body)
	}
	if !strings.Contains(string(body), "product_news:e=false:r=false") {
		t.Errorf("expected product_news back off: %s", body)
	}
}
