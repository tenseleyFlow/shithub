// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"context"
	"net/http"
	"time"
)

// Timeout returns middleware that enforces a per-request timeout via
// context cancellation. Handlers must respect the request context.
//
// Streaming routes (git protocol in S12) MUST be exempt from this
// middleware via a separate route group; the timeout-aware ResponseWriter
// breaks Flusher and would corrupt streaming pack data.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
