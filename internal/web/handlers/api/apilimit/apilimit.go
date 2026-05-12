// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apilimit is the per-request rate limiter that fronts /api/v1/.
// It does two things on every request:
//
//  1. Buckets the caller: PAT-authenticated requests are keyed by token
//     id (scope "api:authed"); anonymous requests are keyed by remote
//     IP (scope "api:anon"). Budgets come from cfg.RateLimit.API.
//  2. Stamps X-RateLimit-Limit / X-RateLimit-Remaining / X-RateLimit-Reset
//     on the response — even on success. The shithub-cli HTTP client
//     parses these headers on every response to surface back-off hints
//     (shithub-cli/internal/api/errors.go).
//
// On deny, the response is the canonical /api/v1 JSON error envelope
// `{"error": "rate limit exceeded"}` with Retry-After set. Postgres
// errors fail open (ratelimit.Allow's documented behavior); the request
// proceeds with whatever decision we have and a warn-level log line.
package apilimit

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// Config is the per-instance configuration for the middleware. Both
// budgets are required to be positive at construction.
type Config struct {
	// AuthedPerHour is the bucket size for PAT-authenticated callers.
	AuthedPerHour int
	// AnonPerHour is the bucket size for unauthenticated callers.
	AnonPerHour int
	// Logger receives warn-level lines when the backing counter errors.
	// nil disables logging.
	Logger *slog.Logger
}

// Middleware returns a chi-compatible middleware that applies the
// configured budgets and stamps the standard X-RateLimit-* headers.
// When l is nil the middleware is a no-op (used by tests that don't
// stand up the ratelimit DB).
func Middleware(l *ratelimit.Limiter, cfg Config) func(http.Handler) http.Handler {
	authedPolicy := ratelimit.Policy{
		Scope:  "api:authed",
		Max:    cfg.AuthedPerHour,
		Window: time.Hour,
	}
	anonPolicy := ratelimit.Policy{
		Scope:  "api:anon",
		Max:    cfg.AnonPerHour,
		Window: time.Hour,
	}
	logger := cfg.Logger
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l == nil {
				next.ServeHTTP(w, r)
				return
			}
			policy, key := pickBucket(r, authedPolicy, anonPolicy)
			if policy.Max <= 0 || key == "" {
				// Misconfigured budget or no key derivable — fail open
				// rather than refuse service. The boot-time validation
				// in config.Validate keeps this branch unreachable in
				// practice.
				next.ServeHTTP(w, r)
				return
			}
			decision, err := l.Allow(r.Context(), policy, key)
			if err != nil && logger != nil {
				logger.WarnContext(r.Context(), "apilimit: counter error", "scope", policy.Scope, "key", key, "error", err)
			}
			ratelimit.StampHeaders(w, decision)
			if !decision.Allowed {
				retry := int(decision.RetryAfter / time.Second)
				if retry < 1 {
					retry = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":"rate limit exceeded"}` + "\n"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func pickBucket(r *http.Request, authed, anon ratelimit.Policy) (ratelimit.Policy, string) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.TokenID != 0 {
		return authed, "pat:" + strconv.FormatInt(auth.TokenID, 10)
	}
	ip := middleware.RealIPFromContext(r.Context(), r)
	if ip == "" {
		return anon, ""
	}
	return anon, "ip:" + ip
}
