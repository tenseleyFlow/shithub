// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const (
	//nolint:gosec // G101 false positive: test fixture, not a real credential.
	pwOriginal = "correct horse battery staple"
	//nolint:gosec // G101 false positive: test fixture, not a real credential.
	pwNew = "tr0ub4dor & 3 horseshoes"
)

func loginPasswordUser(t *testing.T, name string) *client {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)
	mustSignup(t, cli, name, name+"@example.com", pwOriginal)
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {name},
		"password":   {pwOriginal},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli
}

func TestPasswordChange_Roundtrip(t *testing.T) {
	t.Parallel()
	cli := loginPasswordUser(t, "pwa")
	csrf := cli.extractCSRF(t, "/settings/password")
	resp := cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {pwNew},
		"confirm_password": {pwNew},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Password updated") {
		t.Fatalf("expected success, got: %s", body)
	}
}

func TestPasswordChange_RejectsWrongCurrent(t *testing.T) {
	t.Parallel()
	cli := loginPasswordUser(t, "pwb")
	csrf := cli.extractCSRF(t, "/settings/password")
	resp := cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {"not-the-right-password"},
		"new_password":     {pwNew},
		"confirm_password": {pwNew},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Current password is incorrect") {
		t.Fatalf("expected incorrect-current error, got: %s", body)
	}
}

func TestPasswordChange_RejectsMismatch(t *testing.T) {
	t.Parallel()
	cli := loginPasswordUser(t, "pwc")
	csrf := cli.extractCSRF(t, "/settings/password")
	resp := cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {pwNew},
		"confirm_password": {pwNew + "X"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	// HTML-escaping turns "don't" into "don&#39;t", so we check for the
	// stable substring "and confirmation".
	if !strings.Contains(string(body), "and confirmation") {
		t.Fatalf("expected confirmation mismatch error, got: %s", body)
	}
}

func TestPasswordChange_RejectsTooShort(t *testing.T) {
	t.Parallel()
	cli := loginPasswordUser(t, "pwd")
	csrf := cli.extractCSRF(t, "/settings/password")
	resp := cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {"short"},
		"confirm_password": {"short"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "at least 10") {
		t.Fatalf("expected too-short error, got: %s", body)
	}
}

func TestPasswordChange_RejectsCommon(t *testing.T) {
	t.Parallel()
	cli := loginPasswordUser(t, "pwe")
	csrf := cli.extractCSRF(t, "/settings/password")
	resp := cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {"qwertyuiop"}, // 10-char common-list entry
		"confirm_password": {"qwertyuiop"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "too common") {
		t.Fatalf("expected too-common error, got: %s", body)
	}
}

func TestPasswordChange_OriginalNoLongerWorks(t *testing.T) {
	t.Parallel()
	cli := loginPasswordUser(t, "pwf")
	csrf := cli.extractCSRF(t, "/settings/password")
	resp := cli.post(t, "/settings/password", url.Values{
		"csrf_token":       {csrf},
		"current_password": {pwOriginal},
		"new_password":     {pwNew},
		"confirm_password": {pwNew},
	})
	_ = resp.Body.Close()

	// New session, attempt login with the OLD password — must fail.
	cli2 := newClient(t, cli.srv)
	csrf = cli2.extractCSRF(t, "/login")
	resp = cli2.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"pwf"},
		"password":   {pwOriginal},
	})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("expected old password to fail; login succeeded")
	}
}
