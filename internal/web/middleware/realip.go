// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net"
	"net/http"
	"strings"
)

var realIPKey = ctxKey{name: "real_ip"}

// RealIPConfig configures the real-IP middleware. Trust nothing by default;
// the operator opts into specific proxy networks.
type RealIPConfig struct {
	// TrustedProxies is the list of CIDR blocks whose X-Forwarded-For values
	// are honored. If empty, the middleware is a no-op (RemoteAddr wins).
	TrustedProxies []*net.IPNet
}

// RealIP returns middleware that resolves the client IP by walking the
// X-Forwarded-For header rightwards, accepting only entries that come from
// a configured trusted-proxy network. The resolved IP is stored in context
// for later middleware (access log, rate limit) to consume.
func RealIP(cfg RealIPConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := resolveRealIP(r, cfg)
			ctx := context.WithValue(r.Context(), realIPKey, ip)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RealIPFromContext returns the real IP set by the middleware, falling back
// to the request's RemoteAddr (host part) when no value has been resolved.
func RealIPFromContext(ctx context.Context, r *http.Request) string {
	if v, ok := ctx.Value(realIPKey).(string); ok && v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func resolveRealIP(r *http.Request, cfg RealIPConfig) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if len(cfg.TrustedProxies) == 0 {
		return host
	}
	if !ipInTrustedNets(host, cfg.TrustedProxies) {
		return host
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return host
	}
	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(parts[i])
		if candidate == "" {
			continue
		}
		if ipInTrustedNets(candidate, cfg.TrustedProxies) {
			continue // skip another proxy hop
		}
		if net.ParseIP(candidate) == nil {
			return host // malformed, fall back
		}
		return candidate
	}
	return host
}

func ipInTrustedNets(ip string, nets []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}
