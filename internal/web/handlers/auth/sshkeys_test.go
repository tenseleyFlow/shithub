// SPDX-License-Identifier: AGPL-3.0-or-later

package auth_test

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func loadFixture(t *testing.T, name string) string {
	t.Helper()
	// Walk up the tree to find the sshkey/testdata fixture.
	candidates := []string{
		filepath.Join("..", "..", "..", "auth", "sshkey", "testdata", name),
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err == nil {
			return string(b)
		}
	}
	t.Fatalf("could not locate fixture %s", name)
	return ""
}

func enrollSSHKeyHelper(t *testing.T) (cli *client) {
	t.Helper()
	httpsrv, captor := newTestServer(t, false)
	cli = newClient(t, httpsrv)

	mustSignup(t, cli, "alicessh", "alicessh@example.com", "correct horse battery staple")
	tok := extractTokenFromMessage(t, captor.all()[0], "/verify-email")
	_ = cli.get(t, "/verify-email/"+tok).Body.Close()

	csrf := cli.extractCSRF(t, "/login")
	resp := cli.post(t, "/login", url.Values{
		"csrf_token": {csrf},
		"username":   {"alicessh"},
		"password":   {"correct horse battery staple"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
	return cli
}

func TestSSHKey_AddListDelete(t *testing.T) {
	t.Parallel()
	cli := enrollSSHKeyHelper(t)
	pub := loadFixture(t, "ed25519.pub")

	// Add.
	csrf := cli.extractCSRF(t, "/settings/keys")
	resp := cli.post(t, "/settings/keys", url.Values{
		"csrf_token": {csrf},
		"title":      {"laptop"},
		"public_key": {pub},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// List body should now contain the fingerprint.
	resp = cli.get(t, "/settings/keys")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	keysRE := regexp.MustCompile(`KEYS=(\d+):(SHA256:[A-Za-z0-9+/=]+);`)
	m := keysRE.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("no key in list body: %s", body)
	}
	id := m[1]

	// Delete.
	csrf = extractCSRFFromBody(t, body)
	resp = cli.post(t, "/settings/keys/"+id+"/delete", url.Values{
		"csrf_token": {csrf},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body2, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete: %d %s", resp.StatusCode, body2)
	}
	_ = resp.Body.Close()

	// List should be empty.
	resp = cli.get(t, "/settings/keys")
	body, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(body), "SHA256:") {
		t.Fatalf("expected empty list, got: %s", body)
	}
}

func TestSSHKey_RejectRSA1024(t *testing.T) {
	t.Parallel()
	cli := enrollSSHKeyHelper(t)
	pub := loadFixture(t, "rsa1024.pub")

	csrf := cli.extractCSRF(t, "/settings/keys")
	resp := cli.post(t, "/settings/keys", url.Values{
		"csrf_token": {csrf},
		"title":      {"weak"},
		"public_key": {pub},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (form re-render)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "2048") {
		t.Fatalf("expected RSA-bits error, got: %s", body)
	}
}

func TestSSHKey_RejectUnparseable(t *testing.T) {
	t.Parallel()
	cli := enrollSSHKeyHelper(t)

	csrf := cli.extractCSRF(t, "/settings/keys")
	resp := cli.post(t, "/settings/keys", url.Values{
		"csrf_token": {csrf},
		"title":      {"bad"},
		"public_key": {"not-a-key"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "couldn") && !strings.Contains(string(body), ".pub") {
		t.Fatalf("expected unparseable error, got: %s", body)
	}
}

func TestSSHKey_DuplicateRejected(t *testing.T) {
	t.Parallel()
	cli := enrollSSHKeyHelper(t)
	pub := loadFixture(t, "ed25519.pub")

	// First add succeeds.
	csrf := cli.extractCSRF(t, "/settings/keys")
	resp := cli.post(t, "/settings/keys", url.Values{
		"csrf_token": {csrf}, "title": {"laptop"}, "public_key": {pub},
	})
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first add: %d %s", resp.StatusCode, body)
	}
	_ = resp.Body.Close()

	// Second add with same key — rejected with friendly error.
	csrf = cli.extractCSRF(t, "/settings/keys")
	resp = cli.post(t, "/settings/keys", url.Values{
		"csrf_token": {csrf}, "title": {"laptop2"}, "public_key": {pub},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dup status: %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "already registered") {
		t.Fatalf("expected dup error, got: %s", body)
	}
}
