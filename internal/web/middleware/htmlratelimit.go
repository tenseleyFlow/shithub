// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/ratelimit"
)

// HTMLRateLimitConfig declares the per-bucket budgets for the
// public-HTML chi group. Numbers are expressed in token-bucket
// terms (Burst + Refill per second) because that's the framing in
// the F02 sprint table; the middleware translates them into the
// fixed-window (Max, Window) shape the backing ratelimit.Limiter
// expects.
//
// Bucket model (per F02 sprint plan):
//
//	Tier            Burst   Refill (1/sec)
//	Anonymous       60      1     → 60 hits / 60s
//	Authenticated   600     10    → 600 hits / 60s
//	Site admin      —       —     bypass entirely
//
// AnonRefill / AuthedRefill must be positive when the
// corresponding Burst is positive; otherwise the constructor's
// boot-time validation will reject the configuration.
type HTMLRateLimitConfig struct {
	AnonBurst    int
	AnonRefill   int
	AuthedBurst  int
	AuthedRefill int
	// Logger receives info-level lines on every throttle hit
	// (`ratelimit.html_throttled`) plus warn-level lines if the
	// backing counter errors out. nil disables logging.
	Logger *slog.Logger
}

// HTMLRateLimit constructs the chi-compatible middleware that
// fronts the public-HTML routes. When the supplied limiter is nil
// the middleware short-circuits to a no-op so tests that don't
// stand up the ratelimit DB stay simple.
//
// On a throttle hit we respond with 429 + Retry-After plus a
// tiny HTML body (or plain text when the client doesn't look
// like a browser). The body is intentionally cheap to render —
// the whole point of the middleware is to keep abusive callers
// from generating expensive renders.
func HTMLRateLimit(l *ratelimit.Limiter, cfg HTMLRateLimitConfig) func(http.Handler) http.Handler {
	anonPolicy := burstToPolicy("html:anon", cfg.AnonBurst, cfg.AnonRefill)
	authedPolicy := burstToPolicy("html:authed", cfg.AuthedBurst, cfg.AuthedRefill)
	logger := cfg.Logger
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if l == nil {
				next.ServeHTTP(w, r)
				return
			}
			// Site admin bypass — operators shouldn't get
			// throttled investigating an incident.
			user := CurrentUserFromContext(r.Context())
			if user.IsSiteAdmin {
				next.ServeHTTP(w, r)
				return
			}
			policy, key := pickHTMLBucket(r, user, anonPolicy, authedPolicy)
			if policy.Max <= 0 || key == "" {
				// Misconfigured (Burst=0) or no key derivable —
				// fail open. Boot-time validation keeps this
				// branch unreachable in practice.
				next.ServeHTTP(w, r)
				return
			}
			decision, err := l.Allow(r.Context(), policy, key)
			if err != nil && logger != nil {
				logger.WarnContext(r.Context(), "htmlratelimit: counter error",
					"scope", policy.Scope, "key", key, "error", err)
			}
			if !decision.Allowed {
				retry := int(decision.RetryAfter / time.Second)
				if retry < 1 {
					retry = 1
				}
				if logger != nil {
					logger.InfoContext(r.Context(), "ratelimit.html_throttled",
						"scope", policy.Scope,
						"key", key,
						"path", r.URL.Path,
						"user_agent", r.UserAgent(),
						"retry_after_seconds", retry)
				}
				writeHTMLThrottle(w, r, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// burstToPolicy maps F02's token-bucket framing onto the backing
// fixed-window limiter. Burst becomes Max; the window is
// Burst / Refill seconds (clamped to at least 1s). 60/1 → 60 hits
// in 60 seconds; 600/10 → 600 hits in 60 seconds.
//
// Returns a zero Policy.Max when burst is non-positive so callers
// can detect the disabled-tier case without a separate flag.
func burstToPolicy(scope string, burst, refillPerSec int) ratelimit.Policy {
	if burst <= 0 || refillPerSec <= 0 {
		return ratelimit.Policy{}
	}
	windowSec := burst / refillPerSec
	if windowSec < 1 {
		windowSec = 1
	}
	return ratelimit.Policy{
		Scope:  scope,
		Max:    burst,
		Window: time.Duration(windowSec) * time.Second,
	}
}

// pickHTMLBucket selects the (policy, key) pair for the request.
// Authenticated viewers bucket by user id; everyone else buckets
// by IP. Returns a zero policy + empty key when no key is
// derivable — the caller treats that as fail-open.
func pickHTMLBucket(r *http.Request, user CurrentUser, anon, authed ratelimit.Policy) (ratelimit.Policy, string) {
	if !user.IsAnonymous() {
		return authed, "user:" + strconv.FormatInt(user.ID, 10)
	}
	ip := RealIPFromContext(r.Context(), r)
	if ip == "" {
		return anon, ""
	}
	return anon, "anon:" + ip
}

// writeHTMLThrottle renders the 429 response body. Browsers get a
// tiny HTML page; everything else (curl, bots that don't send
// text/html in Accept) gets plain text. Either way the response is
// trivially cheap so abusive callers don't push CPU on the deny
// path itself.
func writeHTMLThrottle(w http.ResponseWriter, r *http.Request, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("Cache-Control", "no-store")
	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(htmlThrottleBody))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write([]byte("Too Many Requests. Retry after " + strconv.Itoa(retryAfter) + " seconds.\n"))
}

// wantsHTML reports whether the client looks like a browser. We
// require text/html to appear in Accept explicitly — `*/*` alone
// (curl's default) gets plain text so terminal users see the
// retry hint without HTML markup.
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

const htmlThrottleBody = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Too Many Requests · shithub</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1f2328}h1{margin:0 0 .75rem;font-size:1.25rem}p{margin:.5rem 0}</style>
</head><body>
<h1>Too Many Requests</h1>
<p>shithub has temporarily slowed responses to this client. Wait a minute and try again.</p>
<p>If you're a real person and this is a mistake, <a href="https://github.com/tenseleyFlow/shithub/issues">file an issue</a>.</p>
</body></html>`
