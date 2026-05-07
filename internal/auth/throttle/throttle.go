// SPDX-License-Identifier: AGPL-3.0-or-later

// Package throttle implements counter-based rate limiting for the auth
// surface, backed by the auth_throttle table. The model is fixed-window:
// each (scope, identifier) pair has a hit counter that resets when the
// window has elapsed.
//
// We deliberately use Postgres rather than introducing Redis. At launch
// scale this is well within Postgres's comfort zone, and avoiding a new
// dependency is worth the marginal latency. Migrate if S36 proves it
// necessary.
package throttle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// DBTX matches sqlc's DBTX so callers can pass a *pgxpool.Pool or a
// transaction interchangeably.
type DBTX = usersdb.DBTX

// Limiter holds the queries handle. Construct with NewLimiter.
type Limiter struct {
	q *usersdb.Queries
}

// NewLimiter returns a limiter bound to the sqlc queries package.
func NewLimiter() *Limiter {
	return &Limiter{q: usersdb.New()}
}

// Limit is a (scope, identifier, max-hits, window) policy.
type Limit struct {
	Scope      string        // e.g. "login", "signup", "reset"
	Identifier string        // e.g. "ip:1.2.3.4|alice"
	Max        int           // hits permitted within Window
	Window     time.Duration // sliding-fixed window length
}

// ErrThrottled is returned by Hit when the policy's limit is exceeded.
// RetryAfter carries a hint suitable for the HTTP `Retry-After` header.
type ErrThrottled struct {
	RetryAfter time.Duration
	Hits       int
}

func (e *ErrThrottled) Error() string {
	return fmt.Sprintf("throttle: limit exceeded (hits=%d, retry in %s)", e.Hits, e.RetryAfter)
}

// IsThrottled is the canonical predicate for distinguishing a throttle
// rejection from a generic error.
func IsThrottled(err error) bool {
	var t *ErrThrottled
	return errors.As(err, &t)
}

// Hit increments the counter for the policy. Returns nil if under the
// limit, ErrThrottled (with RetryAfter) if at or over.
//
// The query is conditional: if the existing window started before
// (now - policy.Window) we treat it as a new window and reset hits to 1.
// Otherwise we increment in place. The work happens atomically inside
// Postgres so concurrent requests don't undercount.
func (l *Limiter) Hit(ctx context.Context, db DBTX, p Limit) error {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-p.Window), Valid: true}
	row, err := l.q.BumpAuthThrottle(ctx, db, usersdb.BumpAuthThrottleParams{
		Scope:           p.Scope,
		Identifier:      p.Identifier,
		WindowStartedAt: cutoff,
	})
	if err != nil {
		return fmt.Errorf("throttle: bump: %w", err)
	}
	if int(row.Hits) > p.Max {
		retry := p.Window - time.Since(row.WindowStartedAt.Time)
		if retry < time.Second {
			retry = time.Second
		}
		return &ErrThrottled{RetryAfter: retry, Hits: int(row.Hits)}
	}
	return nil
}

// Reset clears the counter for (scope, identifier). Used after a
// successful login to forgive prior failed attempts on the same key.
func (l *Limiter) Reset(ctx context.Context, db DBTX, scope, identifier string) error {
	return l.q.ResetAuthThrottle(ctx, db, usersdb.ResetAuthThrottleParams{
		Scope:      scope,
		Identifier: identifier,
	})
}

// Purge deletes throttle rows older than cutoff. Caller is expected to
// run this on a periodic schedule (S14 worker).
func (l *Limiter) Purge(ctx context.Context, db DBTX, olderThan time.Duration) error {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-olderThan), Valid: true}
	return l.q.PurgeStaleAuthThrottle(ctx, db, cutoff)
}
