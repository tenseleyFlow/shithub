// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// loginEmailsUser returns an authenticated client AND the captureSender
// so tests can pull verification tokens out of newly-sent emails.
func loginEmailsUser(t *testing.T, name string) (*client, *httptest.Server, *captureSender) {
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
	return cli, httpsrv, captor
}

// extractEmailIDFromList scans the test fixture's "EMAILS=…" payload for
// the row whose address matches addr and returns its ID.
//
// Fixture format:  ID:address:p=<bool>:v=<bool>;…
var emailRowRE = regexp.MustCompile(`(\d+):([^:]+):p=(true|false):v=(true|false);`)

func extractEmailID(t *testing.T, body, addr string) int64 {
	t.Helper()
	for _, m := range emailRowRE.FindAllStringSubmatch(body, -1) {
		if m[2] == addr {
			var id int64
			for _, c := range m[1] {
				id = id*10 + int64(c-'0')
			}
			return id
		}
	}
	t.Fatalf("no email row for %q in body: %s", addr, body)
	return 0
}

func TestEmails_ListShowsPrimary(t *testing.T) {
	t.Parallel()
	cli, _, _ := loginEmailsUser(t, "ema")
	resp := cli.get(t, "/settings/emails")
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "ema@example.com:p=true:v=true") {
		t.Fatalf("expected primary verified row, got: %s", body)
	}
}

func TestEmails_AddSendsVerify(t *testing.T) {
	t.Parallel()
	cli, _, captor := loginEmailsUser(t, "emb")
	captor.reset()

	csrf := cli.extractCSRF(t, "/settings/emails")
	resp := cli.post(t, "/settings/emails", url.Values{
		"csrf_token": {csrf},
		"email":      {"emb-second@example.com"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Verification link sent") {
		t.Fatalf("expected success, got: %s", body)
	}
	if !strings.Contains(string(body), "emb-second@example.com:p=false:v=false") {
		t.Fatalf("new email row missing or wrong shape: %s", body)
	}
	if len(captor.all()) == 0 {
		t.Fatalf("expected captor to have a verify email")
	}
}

func TestEmails_AddRejectsBad(t *testing.T) {
	t.Parallel()
	cli, _, _ := loginEmailsUser(t, "emc")
	csrf := cli.extractCSRF(t, "/settings/emails")
	resp := cli.post(t, "/settings/emails", url.Values{
		"csrf_token": {csrf},
		"email":      {"not-an-email"},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "valid email") {
		t.Fatalf("expected validation, got: %s", body)
	}
}

func TestEmails_SetPrimaryRequiresVerified(t *testing.T) {
	t.Parallel()
	cli, _, _ := loginEmailsUser(t, "emd")
	csrf := cli.extractCSRF(t, "/settings/emails")
	resp := cli.post(t, "/settings/emails", url.Values{
		"csrf_token": {csrf},
		"email":      {"emd-second@example.com"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	id := extractEmailID(t, string(body), "emd-second@example.com")
	csrf = cli.extractCSRF(t, "/settings/emails")
	resp = cli.post(t, "/settings/emails/"+itoa(id)+"/primary", url.Values{
		"csrf_token": {csrf},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Verify the address") {
		t.Fatalf("expected verify-required error, got: %s", body)
	}
}

func TestEmails_RoundtripVerifySetPrimaryRemove(t *testing.T) {
	t.Parallel()
	cli, _, captor := loginEmailsUser(t, "eme")
	captor.reset()

	// 1. Add a second address.
	csrf := cli.extractCSRF(t, "/settings/emails")
	resp := cli.post(t, "/settings/emails", url.Values{
		"csrf_token": {csrf},
		"email":      {"eme-second@example.com"},
	})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	id := extractEmailID(t, string(body), "eme-second@example.com")

	// 2. Verify via the link in the captured email.
	if len(captor.all()) == 0 {
		t.Fatalf("expected verify email; none captured")
	}
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	// 3. Promote it to primary.
	csrf = cli.extractCSRF(t, "/settings/emails")
	resp = cli.post(t, "/settings/emails/"+itoa(id)+"/primary", url.Values{
		"csrf_token": {csrf},
	})
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "Primary email is now eme-second@example.com") {
		t.Fatalf("expected primary-set success, got: %s", body)
	}
	if !strings.Contains(string(body), "eme-second@example.com:p=true:v=true") {
		t.Fatalf("expected new row to be primary+verified: %s", body)
	}

	// 4. Now the original address can be removed (no longer primary).
	origID := extractEmailID(t, string(body), "eme@example.com")
	csrf = cli.extractCSRF(t, "/settings/emails")
	resp = cli.post(t, "/settings/emails/"+itoa(origID)+"/remove", url.Values{
		"csrf_token": {csrf},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Email removed") {
		t.Fatalf("expected remove success, got: %s", body)
	}
	if strings.Contains(string(body), "eme@example.com:") {
		t.Fatalf("removed email still listed: %s", body)
	}
}

func TestEmails_CannotRemovePrimary(t *testing.T) {
	t.Parallel()
	cli, _, _ := loginEmailsUser(t, "emf")

	// Find the (only) primary row's id.
	resp := cli.get(t, "/settings/emails")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	id := extractEmailID(t, string(body), "emf@example.com")

	csrf := cli.extractCSRF(t, "/settings/emails")
	resp = cli.post(t, "/settings/emails/"+itoa(id)+"/remove", url.Values{
		"csrf_token": {csrf},
	})
	defer func() { _ = resp.Body.Close() }()
	body, _ = io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "set a different primary first") {
		t.Fatalf("expected primary-protected error, got: %s", body)
	}
}

// itoa avoids strconv to keep imports tight.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
