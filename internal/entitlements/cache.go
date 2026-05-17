// SPDX-License-Identifier: AGPL-3.0-or-later

package entitlements

import (
	"context"
	"sync"

	"github.com/tenseleyFlow/shithub/internal/billing"
)

// PRO-EXT_SR2-13 (audit Q3): request-scoped memo for ForPrincipal.
//
// A single HTTP request commonly fans out to multiple entitlement
// gates — repo settings pages call check 3-6 times for the viewer
// (branch protection, required reviewers, advanced settings, contrib
// privacy, ...). Each call previously re-loaded billing state from
// Postgres. The cache below collapses those into one DB round-trip
// per (request × principal).
//
// Lifecycle: web middleware seeds an empty cache on the request
// context; entitlements.ForPrincipal populates it on miss and reads
// from it on hit. Callers without the cache installed (CLI tools,
// background workers, tests) take the un-memoized path — the lookup
// returns ok=false on a missing context key and behavior is unchanged.
//
// Soundness: the cache only outlives one request, so plan / status
// changes that happen mid-request would still be picked up by the
// next request. Billing webhooks (the only mutator) publish
// pagecache invalidations on plan flips; tabs reload + the next
// request sees the new state.

type principalCacheKey struct{}

type principalCache struct {
	mu    sync.Mutex
	store map[billing.Principal]Set
}

// ContextWithPrincipalCache installs an empty memoization cache on
// ctx. Web middleware calls this once per request before handler
// dispatch.
func ContextWithPrincipalCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, principalCacheKey{}, &principalCache{
		store: make(map[billing.Principal]Set, 2),
	})
}

func cacheFromContext(ctx context.Context) *principalCache {
	if v, ok := ctx.Value(principalCacheKey{}).(*principalCache); ok {
		return v
	}
	return nil
}

func lookupPrincipalCache(ctx context.Context, p billing.Principal) (Set, bool) {
	c := cacheFromContext(ctx)
	if c == nil {
		return Set{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	set, ok := c.store[p]
	return set, ok
}

func storePrincipalCache(ctx context.Context, p billing.Principal, set Set) {
	c := cacheFromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store[p] = set
}
