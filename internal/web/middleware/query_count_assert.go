// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/infra/db"
)

// CountQueries returns middleware that wraps every request context
// with db.WithCounter so the per-request query count is observable
// downstream.
//
// Production: install at the router root in test builds only — it
// adds one context.WithValue per request; cheap, but conceptually
// reserved for the test surface.
//
// Test usage:
//
//	r.Use(middleware.CountQueries())
//	rec := httptest.NewRecorder()
//	req := httptest.NewRequest(...)
//	r.ServeHTTP(rec, req)
//	if got := middleware.QueriesFor(req); got > 8 {
//	    t.Fatalf("issuesList ran %d queries; threshold 8", got)
//	}
//
// The threshold is per-route; callers document it in the test.
func CountQueries() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := db.WithCounter(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// QueriesFor reports the number of queries observed against the
// request's context. Returns 0 when CountQueries wasn't installed.
//
// Reads the COUNTER from the request's context after the handler
// has returned; the counter is mutated by goroutines spawned during
// the request, but those should have synchronized before the
// handler returns.
func QueriesFor(r *http.Request) int64 {
	return db.FromContext(r.Context())
}
