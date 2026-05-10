// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ssrf is the canonical SSRF-defense layer (S35). The S33
// webhook deliverer was the first caller; future outbound-fetch
// paths (avatar mirroring, OG-image scraping, OAuth metadata
// fetches, etc.) plug into the same Config + HTTPClient.
//
// The pattern, copied from the webhook package and codified here:
//
//  1. Resolve the hostname.
//  2. Reject the request if ANY resolved IP falls in a private,
//     loopback, link-local, CGNAT, multicast, or ULA range.
//  3. Dial the resolved IP directly so a re-resolve at connect
//     time can't rebind to a private address (DNS rebinding).
//  4. Reject schemes other than http/https and ports outside the
//     operator's allow-list (default 80/443/8080/8443).
//
// Operators with a self-hosted CI behind a private IP set
// AllowedHosts (exact-match, case-insensitive) which implicitly
// allows the host to bypass the IP block-list. Don't confuse with
// AllowPrivateNetworks (global bypass — for testing only).
package ssrf

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

// Error wraps a rejection so callers can `errors.As` to the typed
// shape and surface the specific reason in admin logs / UI.
type Error struct {
	URL    string
	Reason string
}

func (e *Error) Error() string { return fmt.Sprintf("ssrf: %s: %s", e.Reason, e.URL) }

// Is reports whether err is or wraps an *Error.
func Is(err error) bool {
	var s *Error
	return errors.As(err, &s)
}

// Config tunes the defense. Defaults are safe for a single-tenant
// public deployment; self-hosters extend AllowedHosts / AllowedPorts.
type Config struct {
	AllowedSchemes       []string
	AllowedPorts         []int
	AllowPrivateNetworks bool
	AllowedHosts         []string
	DialTimeout          time.Duration
	RequestTimeout       time.Duration
	Resolver             *net.Resolver
}

// Default returns the production defaults.
func Default() Config {
	return Config{
		AllowedSchemes: []string{"http", "https"},
		AllowedPorts:   []int{80, 443, 8080, 8443},
		DialTimeout:    10 * time.Second,
		RequestTimeout: 30 * time.Second,
	}
}

// HTTPClient returns a redirect-following-disabled client whose
// dialer enforces the SSRF rules. 3xx is treated as success-like
// (no-follow) since following is itself an SSRF amplifier.
func (c Config) HTTPClient() *http.Client {
	cfg := c.applyDefaults()
	tr := &http.Transport{
		DialContext:           cfg.dialContext,
		ResponseHeaderTimeout: cfg.RequestTimeout,
		ForceAttemptHTTP2:     false,
		DisableKeepAlives:     true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   cfg.RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateWithResolve runs Validate plus a DNS resolve so callers can
// reject loopback/private/CGNAT/multicast hosts at create-time, not
// only at delivery-time. dialContext still re-resolves on each dial
// (DNS rebinding defense) — this is the cheap-but-thorough gate
// admin forms call so the persisted hook can't sit broken-on-arrival
// (SR2 H3). It's NOT a substitute for the dial-time check.
//
// A nil Resolver uses net.DefaultResolver. AllowedHosts (exact match
// case-insensitive) bypasses the IP block-list as in dialContext.
func (c Config) ValidateWithResolve(ctx context.Context, rawURL string) error {
	if err := c.Validate(rawURL); err != nil {
		return err
	}
	cfg := c.applyDefaults()
	u, err := url.Parse(rawURL)
	if err != nil {
		return &Error{URL: rawURL, Reason: "malformed URL"}
	}
	host := u.Hostname()
	if stringSetContainsFold(cfg.AllowedHosts, host) {
		return nil
	}
	if cfg.AllowPrivateNetworks {
		return nil
	}
	// IP literal — check directly without DNS.
	if ip := net.ParseIP(host); ip != nil {
		if IsForbiddenIP(ip) {
			return &Error{URL: rawURL, Reason: "host resolves to forbidden IP " + ip.String()}
		}
		return nil
	}
	// Hostname — resolve and check every result.
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return &Error{URL: rawURL, Reason: "DNS resolve: " + err.Error()}
	}
	if len(ips) == 0 {
		return &Error{URL: rawURL, Reason: "no IPs resolved"}
	}
	for _, ipa := range ips {
		if IsForbiddenIP(ipa.IP) {
			return &Error{URL: rawURL, Reason: "host resolves to forbidden IP " + ipa.IP.String()}
		}
	}
	return nil
}

// Validate runs the syntactic gate (scheme, port, host shape) without
// resolving DNS. dialContext re-runs the IP-level checks at connect
// time so a passing Validate doesn't imply the URL is safe to dial —
// it's the early-rejection cheap gate. For full create-time rejection
// (loopback, private IPs, etc.) use ValidateWithResolve.
func (c Config) Validate(rawURL string) error {
	cfg := c.applyDefaults()
	u, err := url.Parse(rawURL)
	if err != nil {
		return &Error{URL: rawURL, Reason: "malformed URL"}
	}
	if !stringSetContains(cfg.AllowedSchemes, u.Scheme) {
		return &Error{URL: rawURL, Reason: "scheme " + u.Scheme + " not allowed"}
	}
	host := u.Hostname()
	if host == "" {
		return &Error{URL: rawURL, Reason: "missing host"}
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
		return &Error{URL: rawURL, Reason: "invalid port"}
	}
	if !intSetContains(cfg.AllowedPorts, pn) {
		return &Error{URL: rawURL, Reason: "port " + port + " not in allow-list"}
	}
	return nil
}

func (c Config) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &Error{URL: addr, Reason: "split host:port: " + err.Error()}
	}
	pn, _ := strconv.Atoi(port)
	if !intSetContains(c.AllowedPorts, pn) {
		return nil, &Error{URL: addr, Reason: "port " + port + " not in allow-list"}
	}
	hostAllowed := stringSetContainsFold(c.AllowedHosts, host)
	resolver := c.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	ips, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, &Error{URL: addr, Reason: "DNS resolve: " + err.Error()}
	}
	if len(ips) == 0 {
		return nil, &Error{URL: addr, Reason: "no IPs resolved"}
	}
	for _, ipa := range ips {
		if !hostAllowed && !c.AllowPrivateNetworks && IsForbiddenIP(ipa.IP) {
			return nil, &Error{URL: addr, Reason: "resolved to forbidden IP " + ipa.IP.String()}
		}
	}
	dialer := &net.Dialer{Timeout: c.DialTimeout}
	dialAddr := net.JoinHostPort(ips[0].IP.String(), port)
	return dialer.DialContext(ctx, network, dialAddr)
}

func (c Config) applyDefaults() Config {
	def := Default()
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

// IsForbiddenIP reports whether the IP belongs to any of the ranges
// the spec marks as off-limits. Exposed so callers running their own
// validation (e.g. a config-time URL probe) can reuse the check.
func IsForbiddenIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() {
		return true
	}
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
