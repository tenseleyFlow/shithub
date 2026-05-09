// SPDX-License-Identifier: AGPL-3.0-or-later

package openredirect

import "testing"

func TestSafe_AcceptsRelativePaths(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	for _, ok := range []string{
		"/",
		"/owner/repo",
		"/owner/repo?tab=issues",
		"/owner/repo/issues/1#comment-3",
	} {
		if !cfg.Safe(ok) {
			t.Errorf("Safe(%q) = false; want true", ok)
		}
	}
}

func TestSafe_RejectsHostHopAttempts(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	for _, bad := range []string{
		"",
		"//evil.tld/x",      // protocol-relative
		`/\evil.tld/x`,      // backslash trick
		"http://evil.tld/x", // absolute, no allow-list
		"https://evil.tld/x",
		"javascript:alert(1)",
		"data:text/html,<script>x</script>",
		"file:///etc/passwd",
		"owner/repo", // missing leading slash
	} {
		if cfg.Safe(bad) {
			t.Errorf("Safe(%q) = true; want false", bad)
		}
	}
}

func TestSafe_AcceptsAllowedHost(t *testing.T) {
	t.Parallel()
	cfg := Config{AllowedHosts: []string{"shithub.example", "WWW.shithub.example"}}
	for _, ok := range []string{
		"https://shithub.example/owner/repo",
		"https://www.shithub.example/foo",
		"http://shithub.example/foo",
	} {
		if !cfg.Safe(ok) {
			t.Errorf("Safe(%q) = false; want true (allow-listed)", ok)
		}
	}
}

func TestSafe_RejectsUnlistedHostEvenForHTTPS(t *testing.T) {
	t.Parallel()
	cfg := Config{AllowedHosts: []string{"shithub.example"}}
	for _, bad := range []string{
		"https://attacker.example/foo",
		"https://shithub.example.attacker.tld/foo",
	} {
		if cfg.Safe(bad) {
			t.Errorf("Safe(%q) = true; want false", bad)
		}
	}
}

func TestSafeOr(t *testing.T) {
	t.Parallel()
	cfg := Config{}
	if got := cfg.SafeOr("/owner/repo", "/"); got != "/owner/repo" {
		t.Errorf("SafeOr good = %q; want /owner/repo", got)
	}
	if got := cfg.SafeOr("//evil", "/"); got != "/" {
		t.Errorf("SafeOr bad = %q; want /", got)
	}
}
