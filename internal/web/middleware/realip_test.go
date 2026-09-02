// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustNets(t *testing.T, cidrs ...string) []*net.IPNet {
	t.Helper()
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			t.Fatalf("ParseCIDR(%q): %v", c, err)
		}
		out = append(out, n)
	}
	return out
}

// serveRealIP runs the middleware and reports what
// RealIPFromContext saw — the value every downstream rate limiter
// keys on.
func serveRealIP(t *testing.T, cfg RealIPConfig, remoteAddr string, xff string) string {
	t.Helper()
	var got string
	h := RealIP(cfg)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = RealIPFromContext(r.Context(), r)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestRealIP(t *testing.T) {
	t.Parallel()

	loopback := RealIPConfig{TrustedProxies: mustNets(t, "127.0.0.0/8", "::1/128")}

	tests := []struct {
		name       string
		cfg        RealIPConfig
		remoteAddr string
		xff        string
		want       string
	}{
		{
			// The production shape: Caddy reverse-proxies from
			// loopback and appends the client to XFF.
			name:       "trusted proxy honours xff",
			cfg:        loopback,
			remoteAddr: "127.0.0.1:54321",
			xff:        "203.0.113.7",
			want:       "203.0.113.7",
		},
		{
			// A forged XFF from a direct caller must not move it
			// into another client's rate-limit bucket.
			name:       "untrusted remote ignores xff",
			cfg:        loopback,
			remoteAddr: "198.51.100.4:1234",
			xff:        "203.0.113.7",
			want:       "198.51.100.4",
		},
		{
			// Multi-hop: walk rightwards past our own trusted
			// hops and stop at the first address we did not add.
			name:       "multi-hop picks right-most untrusted",
			cfg:        RealIPConfig{TrustedProxies: mustNets(t, "127.0.0.0/8", "10.0.0.0/8")},
			remoteAddr: "127.0.0.1:54321",
			xff:        "203.0.113.7, 198.51.100.9, 10.0.0.5",
			want:       "198.51.100.9",
		},
		{
			name:       "malformed xff falls back to remote addr",
			cfg:        loopback,
			remoteAddr: "127.0.0.1:54321",
			xff:        "not-an-ip",
			want:       "127.0.0.1",
		},
		{
			name:       "empty xff falls back to remote addr",
			cfg:        loopback,
			remoteAddr: "127.0.0.1:54321",
			want:       "127.0.0.1",
		},
		{
			// No trust list = the pre-fix behaviour, kept as an
			// explicit opt-out for a direct-to-internet deploy.
			name:       "no trusted proxies ignores xff",
			cfg:        RealIPConfig{},
			remoteAddr: "127.0.0.1:54321",
			xff:        "203.0.113.7",
			want:       "127.0.0.1",
		},
		{
			name:       "ipv6 loopback proxy honours xff",
			cfg:        loopback,
			remoteAddr: "[::1]:54321",
			xff:        "2001:db8::99",
			want:       "2001:db8::99",
		},
		{
			// Every hop trusted means nothing in the chain is a
			// client; fall back rather than trust a spoofable head.
			name:       "all hops trusted falls back",
			cfg:        loopback,
			remoteAddr: "127.0.0.1:54321",
			xff:        "127.0.0.2, 127.0.0.3",
			want:       "127.0.0.1",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := serveRealIP(t, tc.cfg, tc.remoteAddr, tc.xff); got != tc.want {
				t.Errorf("real IP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRealIP_SeparatesRateLimitKeys is the regression the fix exists
// for: two anonymous clients arriving through the same loopback proxy
// must land in two different HTML-limiter buckets.
func TestRealIP_SeparatesRateLimitKeys(t *testing.T) {
	t.Parallel()

	cfg := RealIPConfig{TrustedProxies: mustNets(t, "127.0.0.0/8")}
	keys := make(map[string]struct{})
	for _, client := range []string{"203.0.113.7", "198.51.100.4"} {
		ip := serveRealIP(t, cfg, "127.0.0.1:54321", client)
		keys["anon:"+ip] = struct{}{}
	}
	if len(keys) != 2 {
		t.Fatalf("distinct anon bucket keys = %d, want 2 (%v)", len(keys), keys)
	}
}
