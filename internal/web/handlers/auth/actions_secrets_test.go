// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// PRO-EXT01-12: settings handler contract for user-scope Actions
// secrets and variables.
//
//   - Free user, enforce off → write succeeds (report-only).
//   - Free user, enforce on  → write rejected, upgrade banner shown.
//   - Pro user, enforce on   → write succeeds.
//   - Secret listing never exposes plaintext (we list names only).
//   - Variable listing exposes plaintext value (they're not secret).

func postActionsSecret(t *testing.T, cli *client, form url.Values) string {
	t.Helper()
	form.Set("csrf_token", cli.extractCSRF(t, "/settings/actions/secrets"))
	resp := cli.post(t, "/settings/actions/secrets", form)
	defer func() { _ = resp.Body.Close() }()
	// On success the handler 303s back to the listing.
	if resp.StatusCode == http.StatusSeeOther {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func postActionsVariable(t *testing.T, cli *client, form url.Values) string {
	t.Helper()
	form.Set("csrf_token", cli.extractCSRF(t, "/settings/actions/secrets"))
	resp := cli.post(t, "/settings/actions/variables", form)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusSeeOther {
		return ""
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func getActionsSecretsPage(t *testing.T, cli *client) string {
	t.Helper()
	resp := cli.get(t, "/settings/actions/secrets")
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings/actions/secrets: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestActionsSecrets_FreeReportOnlyAllowsCreate(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "alice12reportonly")

	if out := postActionsSecret(t, cli, url.Values{
		"name":  {"DEPLOY_KEY"},
		"value": {"super-secret-string"},
	}); out != "" {
		t.Fatalf("report-only create should succeed; body=%s", out)
	}

	body := getActionsSecretsPage(t, cli)
	if !strings.Contains(body, "SECRETS=DEPLOY_KEY;") {
		t.Errorf("expected secret listed; body=%s", body)
	}
	// Plaintext must never appear in the listing.
	if strings.Contains(body, "super-secret-string") {
		t.Errorf("plaintext leaked into listing: %s", body)
	}
}

func TestActionsSecrets_FreeEnforceRejectsCreate(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{
		EnforceUserActionsSecrets: true,
	})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "alice12enforce")

	out := postActionsSecret(t, cli, url.Values{
		"name":  {"DEPLOY_KEY"},
		"value": {"super-secret-string"},
	})
	if out == "" {
		t.Fatalf("enforce-on Free create should NOT 303 success")
	}
	if !strings.Contains(strings.ToLower(out), "pro") {
		t.Errorf("rejection should mention Pro upgrade; body=%s", out)
	}

	// Listing should show no rows.
	body := getActionsSecretsPage(t, cli)
	if strings.Contains(body, "SECRETS=DEPLOY_KEY;") {
		t.Errorf("enforce-on Free should not have minted; body=%s", body)
	}
}

func TestActionsSecrets_RejectsInvalidName(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "alice12badname")

	out := postActionsSecret(t, cli, url.Values{
		"name":  {"123-bad-start"},
		"value": {"x"},
	})
	if out == "" {
		t.Fatalf("invalid name should NOT 303 success")
	}
	if !strings.Contains(strings.ToLower(out), "name") {
		t.Errorf("rejection should mention name validation; body=%s", out)
	}
}

func TestActionsSecrets_RejectsEmptyValue(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "alice12emptyval")

	out := postActionsSecret(t, cli, url.Values{
		"name":  {"DEPLOY_KEY"},
		"value": {""},
	})
	if out == "" {
		t.Fatalf("empty value should NOT 303 success")
	}
}

func TestActionsVariables_CreateAndDelete(t *testing.T) {
	t.Parallel()
	srv, pool, captor := newTestServerWithPoolOptions(t, authTestOptions{})
	cli, _ := newBillingTestUser(t, srv, pool, captor, "alice12vars")

	if out := postActionsVariable(t, cli, url.Values{
		"name":  {"REGISTRY_HOST"},
		"value": {"ghcr.io"},
	}); out != "" {
		t.Fatalf("variable create should 303; body=%s", out)
	}

	body := getActionsSecretsPage(t, cli)
	if !strings.Contains(body, "VARS=REGISTRY_HOST=ghcr.io;") {
		t.Errorf("expected variable listed with value; body=%s", body)
	}

	// Delete it.
	csrf := cli.extractCSRF(t, "/settings/actions/secrets")
	resp := cli.post(t, "/settings/actions/variables/REGISTRY_HOST/delete", url.Values{
		"csrf_token": {csrf},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: got %d, want 303", resp.StatusCode)
	}

	body = getActionsSecretsPage(t, cli)
	if strings.Contains(body, "VARS=REGISTRY_HOST=") {
		t.Errorf("variable should be gone after delete; body=%s", body)
	}
}
