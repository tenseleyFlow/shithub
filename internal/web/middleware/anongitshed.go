// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

// AnonGitShedConfig bounds how many anonymous requests may sit inside
// git-backed read handlers at once.
//
// The HTML rate limit (htmlratelimit.go) budgets per client network.
// That stops a crawler working from one allocation, but not one that
// arrives through a residential-proxy pool: the 2026-11-25 crawl sent
// 45k distinct addresses in ten minutes, almost all making a single
// request for /{owner}/{repo}/commits/<sha>. No per-client key bounds
// that. Every one of those requests forks git, so the box ran out of
// CPU and unrelated pages (/explore, /) timed out behind them.
//
// This gate is keyed on nothing: it caps the total anonymous
// concurrency on the expensive routes and sheds the overflow with a
// cheap 503. Signed-in users are never gated, and routes that do not
// fork git are never gated, so the rest of the site stays up while a
// crawl is in progress.
type AnonGitShedConfig struct {
	// Slots is the number of anonymous git-backed requests allowed
	// to run concurrently. Zero or negative disables the gate.
	Slots int
	// Queue is how many further requests may wait for a slot. A
	// short queue absorbs a browser fetching a page's handful of
	// sub-resources at once; anything deeper is shed immediately so
	// a flood cannot park thousands of goroutines.
	Queue int
	// QueueWait is the longest a queued request waits before it is
	// shed.
	QueueWait time.Duration
	// RetryAfter is advertised on the 503.
	RetryAfter time.Duration
}

// DefaultAnonGitShedConfig derives the queue shape from the slot
// count: four waiters per slot, two seconds of patience.
func DefaultAnonGitShedConfig(slots int) AnonGitShedConfig {
	return AnonGitShedConfig{
		Slots:      slots,
		Queue:      slots * 4,
		QueueWait:  2 * time.Second,
		RetryAfter: 10 * time.Second,
	}
}

// AnonGitShed returns the load-shedding middleware. It must run after
// OptionalUser (it reads the current user) and should run before the
// HTML rate limit so a shed request costs no database write.
func AnonGitShed(cfg AnonGitShedConfig) func(http.Handler) http.Handler {
	if cfg.Slots <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	retry := int(cfg.RetryAfter / time.Second)
	if retry < 1 {
		retry = 1
	}
	slots := make(chan struct{}, cfg.Slots)
	var waiting atomic.Int64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !anonGitShedApplies(r) {
				next.ServeHTTP(w, r)
				return
			}
			select {
			case slots <- struct{}{}:
				metrics.AnonGitRequestsTotal.WithLabelValues("admitted").Inc()
			default:
				if !waitForGitSlot(r, slots, &waiting, cfg) {
					metrics.AnonGitRequestsTotal.WithLabelValues("shed").Inc()
					writeGitShed(w, r, retry)
					return
				}
				metrics.AnonGitRequestsTotal.WithLabelValues("queued").Inc()
			}
			defer func() { <-slots }()
			next.ServeHTTP(w, r)
		})
	}
}

// waitForGitSlot parks the request in the bounded queue. It reports
// whether a slot was acquired; false means the queue was full, the
// wait expired, or the client went away.
func waitForGitSlot(r *http.Request, slots chan struct{}, waiting *atomic.Int64, cfg AnonGitShedConfig) bool {
	if cfg.Queue <= 0 || cfg.QueueWait <= 0 {
		return false
	}
	if waiting.Add(1) > int64(cfg.Queue) {
		waiting.Add(-1)
		return false
	}
	defer waiting.Add(-1)
	timer := time.NewTimer(cfg.QueueWait)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-r.Context().Done():
		return false
	}
}

// anonGitShedApplies reports whether the request is an anonymous read
// of a git-backed route.
func anonGitShedApplies(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !isGitBackedReadPath(r.URL.Path) {
		return false
	}
	return CurrentUserFromContext(r.Context()).IsAnonymous()
}

// isGitBackedReadPath matches /{owner}/{repo}/{kind}/... for the
// route kinds whose handlers shell out to git on every request. It is
// matched on the path rather than the chi route pattern because the
// gate sits in the group middleware stack, ahead of the rate limiter.
// A false positive only puts a cheap anonymous request through the
// same queue, so the match errs on the side of simplicity.
//
// The repo home page (/{owner}/{repo}) is deliberately left out: it
// is the landing page for every shared link and must stay reachable.
func isGitBackedReadPath(p string) bool {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 4)
	if len(parts) < 4 || parts[0] == "" || parts[1] == "" || parts[3] == "" {
		return false
	}
	switch parts[2] {
	case "commits", "commit", "blob", "raw", "tree", "blame", "compare", "find":
		return true
	}
	return false
}

// writeGitShed renders the 503. Like the throttle body, it is static
// so the deny path costs nothing to produce.
func writeGitShed(w http.ResponseWriter, r *http.Request, retryAfter int) {
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	w.Header().Set("Cache-Control", "no-store")
	if wantsHTML(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(gitShedBody))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("Service busy. Retry after " + strconv.Itoa(retryAfter) + " seconds, or sign in.\n"))
}

const gitShedBody = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Busy · shithub</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:4rem auto;padding:0 1rem;color:#1f2328}h1{margin:0 0 .75rem;font-size:1.25rem}p{margin:.5rem 0}</style>
</head><body>
<h1>shithub is busy</h1>
<p>Too many signed-out visitors are browsing repository history right now. Try again in a few seconds.</p>
<p><a href="/login">Sign in</a> to skip the queue.</p>
</body></html>`
