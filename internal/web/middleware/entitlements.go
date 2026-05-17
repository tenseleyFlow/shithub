// SPDX-License-Identifier: AGPL-3.0-or-later

package middleware

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
)

// EntitlementsCache attaches a per-request memo so
// entitlements.ForPrincipal can collapse the 3-6 calls per request
// that settings + repo pages routinely fan out into one DB round-trip
// per (request × principal). PRO-EXT_SR2-13 (audit Q3).
//
// Wire this once near the top of the chain, after session loading.
// Like PolicyCache, the memo is invalidated naturally by ending the
// request — a billing webhook landing mid-request still wins on the
// next request because pagecache invalidation forces the client to
// reload.
func EntitlementsCache() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := entitlements.ContextWithPrincipalCache(r.Context())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
