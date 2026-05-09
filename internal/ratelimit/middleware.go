// SPDX-License-Identifier: AGPL-3.0-or-later

package ratelimit

import (
	"net/http"
	"strconv"
	"time"
)

// KeyFunc derives the per-request rate-limit key. Common choices:
// remote IP (anon), user id (authed), token id (PAT), repo id+user
// (per-repo), etc. Returning "" skips the limiter (caller should
// only return "" when there's nothing meaningful to throttle on).
type KeyFunc func(*http.Request) string

// Middleware wraps next with a per-request Allow check. On allow,
// X-RateLimit-* headers are stamped on the response; on deny, the
// response is 429 with Retry-After.
//
// `name` is the per-route identifier surfaced in logs and lets the
// admin observability pages disambiguate scopes that share the same
// counter (e.g. "api:anon" used by both /api/repos and /api/users).
func (l *Limiter) Middleware(p Policy, key KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := ""
			if key != nil {
				k = key(r)
			}
			if k == "" {
				next.ServeHTTP(w, r)
				return
			}
			d, err := l.Allow(r.Context(), p, k)
			if err != nil {
				// Fail-open on counter errors — surface in logs via
				// the wrapped handler's logging middleware. We still
				// stamp the headers we know.
				StampHeaders(w, d)
				next.ServeHTTP(w, r)
				return
			}
			StampHeaders(w, d)
			if !d.Allowed {
				w.Header().Set("Retry-After", strconv.Itoa(int(d.RetryAfter/time.Second)))
				// Headers MUST be set before WriteHeader for the
				// response Content-Type to land; matches the same
				// pattern as the writeRetryAfter fix from S05.
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("Rate limit exceeded. Please retry after the X-RateLimit-Reset window.\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// StampHeaders writes the standard X-RateLimit-* headers from a
// Decision. Exposed so handlers that run a manual Allow can apply
// the same stamp without going through Middleware.
func StampHeaders(w http.ResponseWriter, d Decision) {
	if d.Limit > 0 {
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(d.Limit))
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(d.Remaining))
		w.Header().Set("X-RateLimit-Reset", strconv.Itoa(int(d.ResetIn/time.Second)))
	}
}
