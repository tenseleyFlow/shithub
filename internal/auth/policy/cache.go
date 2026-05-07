// SPDX-License-Identifier: AGPL-3.0-or-later

package policy

import (
	"context"
	"sync"
)

// cacheKey is the per-(actor, repo) composite the cache memoizes
// effective role lookups against. The cache is request-scoped — created
// in WithCache, dropped when the context goes out of scope.
type cacheKey struct {
	actorUserID int64
	repoID      int64
}

// requestCache holds the memo. The lock is held across DB lookups so
// concurrent goroutines reading the same key (rare in practice — a
// single request pipeline is typically sequential) coalesce into one
// query.
type requestCache struct {
	mu    sync.Mutex
	roles map[cacheKey]Role
}

type cacheCtxKey struct{}

// WithCache returns a derived context that carries a per-request memo.
// Wire this into the request middleware once, and every Can() call on
// downstream handlers benefits from de-duplication. Calling Can with a
// context that has no cache is supported — the lookup just runs every
// time.
func WithCache(ctx context.Context) context.Context {
	c := &requestCache{roles: make(map[cacheKey]Role, 4)}
	return context.WithValue(ctx, cacheCtxKey{}, c)
}

// cacheFromContext returns the cache or nil. Internal helper — Can()
// degrades gracefully when nil.
func cacheFromContext(ctx context.Context) *requestCache {
	c, _ := ctx.Value(cacheCtxKey{}).(*requestCache)
	return c
}

// InvalidateRepo drops cached entries for one repo. Call this from
// handlers that mutate collaborator state and then re-check policy
// inside the same request.
func InvalidateRepo(ctx context.Context, repoID int64) {
	c := cacheFromContext(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.roles {
		if k.repoID == repoID {
			delete(c.roles, k)
		}
	}
}

// cacheGet returns (role, true) when present.
func cacheGet(c *requestCache, k cacheKey) (Role, bool) {
	if c == nil {
		return RoleNone, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.roles[k]
	return r, ok
}

// cachePut stores a role. Safe to call with c == nil.
func cachePut(c *requestCache, k cacheKey, r Role) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.roles[k] = r
}
