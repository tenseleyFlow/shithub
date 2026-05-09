// SPDX-License-Identifier: AGPL-3.0-or-later

package db

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

// QueryCounter is a pgx QueryTracer that increments a per-context
// counter on every Query/QueryRow/Exec. The counter lives on the
// request context, so the tracer is safe to install at pool-config
// time even when most requests don't care.
//
// Use case: handler integration tests assert "this route does ≤ N
// DB queries." The tracer + WithCounter / FromContext + a tiny
// middleware (web/middleware/query_count_assert.go) make that
// assertion a one-liner.
//
// Production runs leave the tracer installed but never call
// WithCounter; the per-conn overhead is one atomic-load per query
// (the tracer reads the context value but the counter is nil so
// no Add fires).
type QueryCounter struct{}

type queryCounterKey struct{}

// counter is the per-context Adder. Atomic so concurrent Query
// invocations on the same request context (rare but possible —
// goroutines per row) don't undercount.
type counter struct {
	n atomic.Int64
}

// WithCounter returns a derived context that records query counts.
// Pair with Read.
func WithCounter(ctx context.Context) context.Context {
	return context.WithValue(ctx, queryCounterKey{}, &counter{})
}

// FromContext reports how many tracer events have fired against ctx.
// Returns 0 when WithCounter wasn't called on this context.
func FromContext(ctx context.Context) int64 {
	c, ok := ctx.Value(queryCounterKey{}).(*counter)
	if !ok {
		return 0
	}
	return c.n.Load()
}

// TraceQueryStart implements pgx.QueryTracer. The counter increments
// at start, not end, so a slow query still counts.
func (QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	if c, ok := ctx.Value(queryCounterKey{}).(*counter); ok {
		c.n.Add(1)
	}
	return ctx
}

// TraceQueryEnd implements pgx.QueryTracer. No-op; the start tick is
// the meaningful event.
func (QueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
