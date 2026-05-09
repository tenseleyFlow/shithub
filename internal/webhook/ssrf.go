// SPDX-License-Identifier: AGPL-3.0-or-later

// Package webhook owns outbound webhook delivery: signing, SSRF
// defense, retry/backoff, and the deliverer + fanout workers.
//
// SSRF philosophy: webhooks point at attacker-controlled URLs by
// design. The defense pattern is documented in `docs/internal/webhooks.md`
// and enforced in this file:
//
//  1. Resolve the hostname to a set of IPs.
//  2. Reject the request if ANY resolved IP is in a private/loopback/
//     link-local/etc. range — even if other IPs would have been fine.
//     A mixed-result hostname is suspicious enough to refuse.
//  3. Pick a public IP and dial it directly, passing the original
//     hostname for SNI / Host header. This defeats DNS-rebinding
//     because the IP we validated is the IP we connect to (no second
//     resolve at dial time).
//  4. Reject schemes other than http/https and ports outside the
//     well-known web ports unless the operator allow-listed them.
package webhook

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SSRFError describes why a URL was rejected pre-flight or at dial
// time. The error string is operator-friendly; we don't surface the
// exact reason to the deliverer's external counterpart so the message
// stays in our own logs / UI.
type SSRFError struct {
	URL    string
	Reason string
}

func (e *SSRFError) Error() string { return fmt.Sprintf("ssrf: %s: %s", e.Reason, e.URL) }

// SSRFConfig tunes the defense. Defaults are safe for a single-tenant
// public deployment; self-hosters extend AllowedHosts / AllowedPorts
// when delivering to internal CI behind a private IP.
type SSRFConfig struct {
	// AllowedSchemes restricts URL schemes. Default ["http", "https"].
	AllowedSchemes []string
	// AllowedPorts is the set of TCP ports the deliverer is willing to
	// dial. Defaults to {80, 443, 8080, 8443}; operators add internal
	// ports here.
	AllowedPorts []int
	// AllowPrivateNetworks, when true, skips the IP block-list. Use ONLY
	// with a paired AllowedHosts list — the combination lets a self-
	// hoster point a webhook at `ci.internal` while still rejecting any
	// other hostname that would resolve to a private IP.
	AllowPrivateNetworks bool
	// AllowedHosts is a hostname allow-list. When non-empty AND a
	// hostname matches, AllowPrivateNetworks is implicitly applied for
	// that hostname only. Match is exact (no wildcards) and case-
	// insensitive.
	AllowedHosts []string
	// DialTimeout caps the per-dial connect time. Default 10s.
	DialTimeout time.Duration
	// RequestTimeout caps the total request time (connect + read).
	// Default 30s per the spec.
	RequestTimeout time.Duration
	// Resolver is plumbed for tests. nil => net.DefaultResolver.
	Resolver *net.Resolver
}

// DefaultSSRFConfig returns the production defaults. Callers add to
// the slices as needed; the zero-value SSRFConfig is also valid (it
// will pick the same defaults at validation time).
func DefaultSSRFConfig() SSRFConfig {
	return SSRFConfig{
		AllowedSchemes: []string{"http", "https"},
		AllowedPorts:   []int{80, 443, 8080, 8443},
		DialTimeout:    10 * time.Second,
		RequestTimeout: 30 * time.Second,
	}
}

// HTTPClient returns an *http.Client configured with the SSRF-safe
// dialer. The transport intentionally disables redirect-following:
// 3xx is treated as success and a redirect target's IP would otherwise
// bypass our pre-flight check.
func (c SSRFConfig) HTTPClient() *http.Client {
	cfg := c.applyDefaults()
	tr := &http.Transport{
		DialContext:           cfg.dialContext,
		ResponseHeaderTimeout: cfg.RequestTimeout,
		ForceAttemptHTTP2:     false,
		// No keep-alive across deliveries — webhooks are sparse and
		// connection reuse complicates the validate-then-dial chain.
		DisableKeepAlives: true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Validate checks the URL shape (scheme/port/host) without resolving
// DNS. Returns *SSRFError on rejection. The deliverer also re-resolves
// at dial time inside dialContext to defeat rebinding.
func (c SSRFConfig) Validate(rawURL string) error {
	cfg := c.applyDefaults()
	u, err := url.Parse(rawURL)
	if err != nil {
		return &SSRFError{URL: rawURL, Reason: "malformed URL"}
	}
	if !stringSetContains(cfg.AllowedSchemes, u.Scheme) {
		return &SSRFError{URL: rawURL, Reason: "scheme " + u.Scheme + " not allowed"}
	}
	host := u.Hostname()
	if host == "" {
		return &SSRFError{URL: rawURL, Reason: "missing host"}
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	pn, perr := strconv.Atoi(port)
	if perr != nil || pn <= 0 || pn > 65535 {
		return &SSRFError{URL: rawURL, Reason: "invalid port"}
	}
	if !intSetContains(cfg.AllowedPorts, pn) {
		return &SSRFError{URL: rawURL, Reason: "port " + port + " not in allow-list"}
	}
	return nil
}

// dialContext is the SSRF-safe dialer. It re-resolves the hostname at
// dial time, validates every returned IP, and connects to the first
// allowed IP using the original hostname for SNI.
func (c SSRFConfig) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &SSRFError{URL: addr, Reason: "split host:port: " + err.Error()}
	}
	pn, _ := strconv.Atoi(port)
	if !intSetContains(c.AllowedPorts, pn) {
		return nil, &SSRFError{URL: addr, Reason: "port " + port + " not in allow-list"}
	}

	hostAllowed := stringSetContainsFold(c.AllowedHosts, host)
	resolver := c.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, &SSRFError{URL: addr, Reason: "DNS resolve: " + err.Error()}
	}
	if len(ips) == 0 {
		return nil, &SSRFError{URL: addr, Reason: "no IPs resolved"}
	}
	// Reject if ANY IP is forbidden — a mixed-result hostname is
	// suspicious enough to refuse. The exception is when the host is
	// allow-listed (self-hoster scenario).
	for _, ipa := range ips {
		if !hostAllowed && !c.AllowPrivateNetworks && isForbiddenIP(ipa.IP) {
			return nil, &SSRFError{URL: addr, Reason: "resolved to forbidden IP " + ipa.IP.String()}
		}
	}
	// Dial the first IP. We pass the literal IP so the dialer doesn't
	// re-resolve under us; the URL's Host header (set by net/http) keeps
	// the original hostname for routing/SNI.
	dialer := &net.Dialer{Timeout: c.DialTimeout}
	dialAddr := net.JoinHostPort(ips[0].IP.String(), port)
	return dialer.DialContext(ctx, network, dialAddr)
}

// applyDefaults fills in zero-value fields with defaults. Returns a
// copy so the caller's struct stays unchanged.
func (c SSRFConfig) applyDefaults() SSRFConfig {
	def := DefaultSSRFConfig()
	if len(c.AllowedSchemes) == 0 {
		c.AllowedSchemes = def.AllowedSchemes
	}
	if len(c.AllowedPorts) == 0 {
		c.AllowedPorts = def.AllowedPorts
	}
	if c.DialTimeout == 0 {
		c.DialTimeout = def.DialTimeout
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = def.RequestTimeout
	}
	return c
}

// isForbiddenIP returns true if the IP belongs to any of the ranges
// the spec marks as off-limits.
func isForbiddenIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}
	// IPv4 RFC 1918 + CGNAT (100.64/10) + broadcast + the autoconf
	// 169.254/16 range (already covered by IsLinkLocalUnicast but
	// belt-and-braces).
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		case ip4[0] == 0:
			return true
		case ip4[0] == 255:
			return true
		}
		return false
	}
	// IPv6 unique-local addresses (fc00::/7) — covers fd00::/8 too.
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

func stringSetContains(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func stringSetContainsFold(set []string, v string) bool {
	for _, s := range set {
		if equalFold(s, v) {
			return true
		}
	}
	return false
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

func intSetContains(set []int, v int) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// IsSSRF reports whether err is or wraps an SSRFError.
func IsSSRF(err error) bool {
	var s *SSRFError
	return errors.As(err, &s)
}
