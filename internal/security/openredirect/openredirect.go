// SPDX-License-Identifier: AGPL-3.0-or-later

// Package openredirect validates `next=` style redirect targets so a
// crafted query parameter can't bounce a logged-in user off-host
// (the canonical phishing primitive). The Safe predicate accepts:
//
//   - relative paths starting with a single `/` (not `//`, not `/\`)
//   - absolute URLs whose host matches an operator-approved allow-list
//
// Anything else is rejected. Callers fall back to a known-safe default
// (typically `/`) on rejection.
package openredirect

import (
	"net/url"
	"strings"
)

// Config governs which absolute redirect targets are allowed. The
// zero value accepts only relative paths — the safest default for
// most surfaces.
type Config struct {
	// AllowedHosts is the exact, case-insensitive host allow-list
	// for absolute redirect targets. Typical content: the deployment's
	// canonical hostname plus any apex/www variants. Empty disables
	// absolute-URL redirects entirely.
	AllowedHosts []string
}

// Safe reports whether candidate is a safe redirect target under cfg.
// Empty input returns false; callers should pre-screen and substitute
// the default themselves.
func (cfg Config) Safe(candidate string) bool {
	if candidate == "" {
		return false
	}
	// Two-leading-slash protocol-relative URLs (`//attacker.tld/x`)
	// are absolute under the browser's resolution; reject early so a
	// later relative-path check doesn't accept them.
	if strings.HasPrefix(candidate, "//") {
		return false
	}
	// Reverse-slash trick (`/\evil.tld`): some browsers normalise the
	// `\` into `/` and treat the result as protocol-relative. Block.
	if strings.HasPrefix(candidate, "/\\") {
		return false
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return false
	}
	// Relative path: scheme empty AND host empty AND starts with `/`.
	if u.Scheme == "" && u.Host == "" {
		return strings.HasPrefix(u.Path, "/")
	}
	// Absolute URL: scheme must be http(s) and host must be allow-listed.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	for _, h := range cfg.AllowedHosts {
		if equalFold(h, host) {
			return true
		}
	}
	return false
}

// SafeOr returns candidate when Safe(candidate), otherwise fallback.
// The single-call convenience for handlers that want to default to
// `/` after rejection.
func (cfg Config) SafeOr(candidate, fallback string) string {
	if cfg.Safe(candidate) {
		return candidate
	}
	return fallback
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
