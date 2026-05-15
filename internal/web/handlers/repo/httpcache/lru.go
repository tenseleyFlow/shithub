// SPDX-License-Identifier: AGPL-3.0-or-later

package httpcache

import (
	"sync"
	"time"

	"github.com/tenseleyFlow/shithub/internal/cache/lru"
)

// PageKey identifies a single rendered page in the commits-list
// cache. The (RepoID, BranchOID) pair is the invalidation lever:
// a push that moves the branch produces a new OID and every Page
// under the new OID misses the cache cleanly without touching the
// old entries (which the 60s TTL retires on its own anyway).
type PageKey struct {
	RepoID    int64
	BranchOID string
	Page      int
}

// PageCache is the in-process LRU for rendered commits-list HTML
// (F01 PR-3). It's a thin wrapper around lru.Cache that adds a
// reverse-index keyed on (RepoID, BranchOID) so PR-4 can invalidate
// every page of a branch atomically on push completion. The 60s
// TTL is the safety net when invalidation doesn't fire (e.g. tests
// without the push worker, or a stretch endpoint that doesn't
// route through push:process).
//
// Concurrency-safe via the embedded lru.Cache's mutex plus a sister
// mutex covering the reverse index. The two locks are taken in a
// fixed order (cache first, then index) so they can't deadlock.
type PageCache struct {
	cache    *lru.Cache[PageKey, []byte]
	indexMu  sync.Mutex
	byBranch map[branchKey]map[int]struct{}
}

// branchKey is the reverse-index key. Identical to PageKey minus
// the page so a single push can find every cached page in O(1).
type branchKey struct {
	RepoID    int64
	BranchOID string
}

// NewPageCache constructs an LRU. capacity caps live entries;
// ttl is the per-entry expiry. Sprint plan: 256 × 60s for the
// commits list (256 × ~50KB = ~12.5MB upper bound).
func NewPageCache(capacity int, ttl time.Duration) *PageCache {
	if capacity <= 0 || ttl <= 0 {
		return nil
	}
	return &PageCache{
		cache:    lru.NewWithTTL[PageKey, []byte](capacity, ttl),
		byBranch: make(map[branchKey]map[int]struct{}),
	}
}

// Get returns the cached HTML bytes for the given page key, or nil
// + false on miss / TTL-expired / nil receiver. Bytes are
// returned by reference — the caller MUST NOT mutate them.
func (c *PageCache) Get(key PageKey) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	return c.cache.Get(key)
}

// Set stores the HTML bytes under the given key and registers the
// page in the reverse index. Nil receiver is a no-op so the
// handler can fail open when the cache wasn't wired (e.g. tests).
func (c *PageCache) Set(key PageKey, body []byte) {
	if c == nil {
		return
	}
	c.cache.Set(key, body)
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	bk := branchKey{RepoID: key.RepoID, BranchOID: key.BranchOID}
	pages, ok := c.byBranch[bk]
	if !ok {
		pages = make(map[int]struct{}, 1)
		c.byBranch[bk] = pages
	}
	pages[key.Page] = struct{}{}
}

// InvalidateBranch drops every cached page under (repoID, oid).
// Called from the push:process worker job (F01 PR-4) on push
// completion; safe to call on a nil receiver. The reverse-index
// entry is removed too so a future push to the same OID doesn't
// see ghost pages from the old generation.
//
// Note: when a push lands the new head OID is different from the
// old one, so invalidating the OLD oid is what's needed — the
// new OID has no entries yet. PR-4's worker passes the
// pre-push OID accordingly.
func (c *PageCache) InvalidateBranch(repoID int64, oid string) {
	if c == nil {
		return
	}
	bk := branchKey{RepoID: repoID, BranchOID: oid}
	c.indexMu.Lock()
	pages, ok := c.byBranch[bk]
	if ok {
		delete(c.byBranch, bk)
	}
	c.indexMu.Unlock()
	if !ok {
		return
	}
	for page := range pages {
		c.cache.Delete(PageKey{RepoID: repoID, BranchOID: oid, Page: page})
	}
}

// Stats forwards the underlying LRU's hit/miss/eviction counters
// for metrics exporters. Nil receiver returns the zero value so
// callers don't have to branch.
func (c *PageCache) Stats() lru.Stats {
	if c == nil {
		return lru.Stats{}
	}
	return c.cache.Stats()
}
