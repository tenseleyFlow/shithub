// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tenseleyFlow/shithub/internal/ratelimit"
)

// OAuthRateLimit fronts the unauth OAuth device-flow JSON endpoints
// (POST /login/device/code, POST /login/oauth/access_token). Distinct
// from HTMLRateLimit because:
//   - bucket is IP-only (no authed-tier; these endpoints are pre-auth);
//   - response is plain-text 429, not HTML — the callers are CLIs;
//   - body is intentionally NOT an RFC 6749 error envelope. A 429 is
//     a transport-layer back-off signal, not an OAuth protocol error
//     (`slow_down` is the OAuth signal, and it's emitted server-side
//     by the devicecode package against the per-grant interval; this
//     middleware throttles abusive per-IP traffic across many grants).
//
// When the limiter is nil or the policy has Max<=0 the middleware is
// a no-op, so tests that don't stand up the ratelimit DB stay simple.
func OAuthRateLimit(l *ratelimit.Limiter, policy ratelimit.Policy, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l == nil || policy.Max <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			ip := RealIPFromContext(r.Context(), r)
			if ip == "" {
				next.ServeHTTP(w, r)
				return
			}
			key := "ip:" + ip
			decision, err := l.Allow(r.Context(), policy, key)
			if err != nil && logger != nil {
				logger.WarnContext(r.Context(), "oauthratelimit: counter error",
					"scope", policy.Scope, "key", key, "error", err)
			}
			if !decision.Allowed {
				retry := int(decision.RetryAfter / time.Second)
				if retry < 1 {
					retry = 1
				}
				if logger != nil {
					logger.InfoContext(r.Context(), "ratelimit.oauth_throttled",
						"scope", policy.Scope,
						"key", key,
						"path", r.URL.Path,
						"user_agent", r.UserAgent(),
						"retry_after_seconds", retry)
				}
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("Too Many Requests. Retry after " + strconv.Itoa(retry) + " seconds.\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// OAuthDeviceCodePolicy is the bucket for POST /login/device/code:
// 5 issues per IP per minute. Tight because every accepted request
// creates a `device_authorizations` row that the sweep eventually
// reaps; absent throttling an attacker can fill the table.
var OAuthDeviceCodePolicy = ratelimit.Policy{
	Scope:  "oauth:device_code",
	Max:    5,
	Window: time.Minute,
}

// OAuthAccessTokenPolicy is the bucket for POST /login/oauth/access_token:
// 50 polls per IP per minute. Looser because the canonical poll cadence
// (5s interval) is 12 req/min — 50 leaves headroom for legitimate
// retries while still cutting off runaway pollers. Note: per-grant
// slow_down enforcement runs in addition to this; the two layers cover
// distinct abuse shapes.
var OAuthAccessTokenPolicy = ratelimit.Policy{
	Scope:  "oauth:access_token",
	Max:    50,
	Window: time.Minute,
}
