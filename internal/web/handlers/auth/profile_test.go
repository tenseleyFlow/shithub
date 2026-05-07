// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func loginProfileUser(t *testing.T) *client {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli := newClient(t, httpsrv)

	mustSignup(t, cli, "alicep", "alicep@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"alicep"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli
}

func TestProfileEditor_Roundtrip(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)

	// GET shows the form.
	resp := cli.get(t, "/settings/profile")
	if resp.StatusCode != 200 {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Public profile") {
		t.Fatalf("missing heading: %s", body)
	}

	csrf := cli.extractCSRF(t, "/settings/profile")
	resp = cli.post(t, "/settings/profile", url.Values{
		"csrf_token":   {csrf},
		"display_name": {"Alice P."},
		"bio":          {"Building things."},
		"location":     {"Berlin"},
		"website":      {"example.com"}, // bare host -> auto https://
		"company":      {"Acme"},
		"pronouns":     {"she/her"},
	})
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post: %d %s", resp.StatusCode, body)
	}
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{"Profile updated.", "Alice P.", "Building things.", "Berlin", "https://example.com", "Acme", "she/her"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("missing %q in body", want)
		}
	}
}

func TestProfileEditor_RejectsBadURL(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/settings/profile", url.Values{
		"csrf_token": {csrf},
		"website":    {"javascript:alert(1)"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "http(s)") {
		t.Fatalf("expected URL error, got: %s", body)
	}
}

func TestProfileEditor_RequiresAuth(t *testing.T) {
	t.Parallel()
	httpsrv, _ := newTestServer(t, false)
	cli := newClient(t, httpsrv)
	resp := cli.get(t, "/settings/profile")
	defer func() { _ = resp.Body.Close() }()
	// RequireUser sends an unauthenticated visitor to /login (303 See Other).
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != http.StatusFound {
		t.Fatalf("expected redirect, got %d", resp.StatusCode)
	}
}

func TestProfileEditor_TooLongBio(t *testing.T) {
	t.Parallel()
	cli := loginProfileUser(t)
	csrf := cli.extractCSRF(t, "/settings/profile")
	resp := cli.post(t, "/settings/profile", url.Values{
		"csrf_token": {csrf},
		"bio":        {strings.Repeat("a", 501)},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Bio is too long") {
		t.Fatalf("expected bio length error, got: %s", body)
	}
}
