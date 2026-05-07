// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

// PolicyCache attaches a per-request memo to ctx so the policy package
// can de-duplicate (actor, repo) → role lookups across the handler
// chain. Wire this once near the top of the chain, after the request
// has been bound to a session.
//
// Mutations that change collaborator state mid-request must call
// policy.InvalidateRepo(ctx, repoID) before re-checking, or the cache
// will return a stale role.
func PolicyCache() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := policy.WithCache(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
