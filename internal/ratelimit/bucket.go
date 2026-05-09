// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ratelimit owns the cross-surface counter used by S35's
// rate-limit middleware. It generalizes S05's auth/throttle pattern
// against the shared rate_limits table so any new surface (API,
// search, git transports) plugs in with a single Allow() call.
//
// Backend: Postgres-backed fixed-window counter via sqlc UPSERT.
// At launch scale this is well within Postgres's comfort zone and
// avoids introducing a Redis dependency. Move only if profiling
// demands it (S36).
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	ratelimitdb "github.com/tenseleyFlow/shithub/internal/ratelimit/sqlc"
)

// Limiter is the package's primary handle. Construct with New.
type Limiter struct {
	q    *ratelimitdb.Queries
	pool *pgxpool.Pool
}

// New wires a Limiter against a pool. The pool is required;
// constructing with nil panics so callers fail at boot, not at
// first request.
func New(pool *pgxpool.Pool) *Limiter {
	if pool == nil {
		panic("ratelimit: nil pool")
	}
	return &Limiter{q: ratelimitdb.New(), pool: pool}
}

// Policy declares a per-(scope, key) limit: at most Max hits within
// the rolling Window.
type Policy struct {
	Scope  string        // e.g. "api:anon", "search", "git:https"
	Max    int           // hits permitted within Window
	Window time.Duration // window length (15s, 1m, 1h, …)
}

// Decision is the verdict from Allow.
type Decision struct {
	Allowed    bool
	Remaining  int           // hits left in the current window (post-increment)
	Limit      int           // == Policy.Max, surfaced for the X-RateLimit-Limit header
	ResetIn    time.Duration // wall-clock until the current window rolls over
	RetryAfter time.Duration // 0 when Allowed; otherwise the wait the client should respect
}

// Allow increments the (scope, key) counter and reports whether the
// caller is under or over the configured Max. Returns the post-
// increment Remaining + the time until the current window rolls.
//
// On a Postgres error the request is allowed (fail-open). The caller
// is expected to log the error; refusing service over a transient
// counter glitch would be worse than the brief over-limit window.
func (l *Limiter) Allow(ctx context.Context, p Policy, key string) (Decision, error) {
	if p.Max <= 0 || p.Window <= 0 {
		return Decision{}, errors.New("ratelimit: Policy.Max and Window must be positive")
	}
	if p.Scope == "" || key == "" {
		return Decision{}, errors.New("ratelimit: Policy.Scope and key must be non-empty")
	}
	row, err := l.q.BumpRateLimit(ctx, l.pool, ratelimitdb.BumpRateLimitParams{
		Scope: p.Scope,
		Key:   key,
		Ttl:   pgtype.Interval{Microseconds: int64(p.Window / time.Microsecond), Valid: true},
	})
	if err != nil {
		return Decision{Allowed: true, Remaining: p.Max, Limit: p.Max, ResetIn: p.Window}, fmt.Errorf("ratelimit: bump: %w", err)
	}

	hits := int(row.Hits)
	resetIn := time.Until(row.WindowStartedAt.Time.Add(p.Window))
	if resetIn < 0 {
		resetIn = 0
	}
	d := Decision{
		Allowed:   hits <= p.Max,
		Limit:     p.Max,
		Remaining: max0(p.Max - hits),
		ResetIn:   resetIn,
	}
	if !d.Allowed {
		d.RetryAfter = resetIn
		if d.RetryAfter <= 0 {
			d.RetryAfter = time.Second
		}
	}
	return d, nil
}

// AllowSignupIP is the inet-keyed sibling of Allow against the
// signup_ip_throttle table. ip is masked to /24 (v4) or /48 (v6)
// so a single residential allocation shares one counter — matches
// GitHub's approach (per-/24 signup throttle).
func (l *Limiter) AllowSignupIP(ctx context.Context, ip netip.Addr, max int, window time.Duration) (Decision, error) {
	if !ip.IsValid() {
		return Decision{}, errors.New("ratelimit: invalid ip")
	}
	row, err := l.q.BumpSignupIPThrottle(ctx, l.pool, ratelimitdb.BumpSignupIPThrottleParams{
		Cidr: maskToNetwork(ip),
		Ttl:  pgtype.Interval{Microseconds: int64(window / time.Microsecond), Valid: true},
	})
	if err != nil {
		return Decision{Allowed: true, Remaining: max, Limit: max, ResetIn: window}, fmt.Errorf("ratelimit: signup bump: %w", err)
	}
	hits := int(row.Hits)
	resetIn := time.Until(row.WindowStartedAt.Time.Add(window))
	if resetIn < 0 {
		resetIn = 0
	}
	d := Decision{
		Allowed:   hits <= max,
		Limit:     max,
		Remaining: max0(max - hits),
		ResetIn:   resetIn,
	}
	if !d.Allowed {
		d.RetryAfter = resetIn
		if d.RetryAfter <= 0 {
			d.RetryAfter = time.Second
		}
	}
	return d, nil
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// maskToNetwork zeros the host bits of ip so per-/24 (v4) or /48 (v6)
// throttle keys collapse to one network row. The choice of /24 + /48
// matches GitHub's reported anti-abuse defaults.
func maskToNetwork(ip netip.Addr) netip.Addr {
	if ip.Is4() {
		b := ip.As4()
		b[3] = 0
		return netip.AddrFrom4(b)
	}
	b := ip.As16()
	for i := 6; i < 16; i++ { // zero everything past /48
		b[i] = 0
	}
	return netip.AddrFrom16(b)
}
