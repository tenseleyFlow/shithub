// SPDX-License-Identifier: AGPL-3.0-or-later

// Package lru is an in-process least-recently-used cache with
// optional TTL + singleflight wrapping. The S36 perf-pass standardizes
// on this package for every cross-request cache (refs, tree, ahead/
// behind, rendered markdown, etc.) so callers don't roll their own
// eviction story.
//
// Two core types:
//
//   - Cache[K,V]    — count-bounded LRU with optional per-entry TTL.
//     The dumb-and-fast variant for value types whose
//     in-memory cost is uniform.
//
//   - SizedCache[K] — byte-bounded LRU. Each entry contributes a
//     caller-supplied size; eviction runs while the
//     total exceeds the cap. For values whose size
//     varies per key (rendered HTML, diff blobs).
//
// Both expose Get / Set / Delete / Len + a Stats accessor for the
// /metrics surface (S36 baseline asserts hit-rate). Hot-key dogpile
// prevention lives one layer up in `Group` (singleflight wrapper).
package lru

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"
)

// Stats is the per-cache hit / miss / eviction counter set. Counters
// are atomic so /metrics can read without locking the cache.
type Stats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
}

// Cache is a count-bounded LRU. Construct with New[K,V](capacity).
type Cache[K comparable, V any] struct {
	mu       sync.Mutex
	capacity int
	ll       *list.List
	items    map[K]*list.Element
	ttl      time.Duration // zero = no TTL
	now      func() time.Time

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

// entry is the linked-list payload. ExpiresAt is zero when the
// cache has no TTL.
type entry[K comparable, V any] struct {
	key       K
	val       V
	expiresAt time.Time
}

// New constructs a count-bounded LRU. capacity must be positive.
// The TTL defaults to "no expiry"; use NewWithTTL to set one.
func New[K comparable, V any](capacity int) *Cache[K, V] {
	if capacity <= 0 {
		panic("lru: capacity must be positive")
	}
	return &Cache[K, V]{
		capacity: capacity,
		ll:       list.New(),
		items:    make(map[K]*list.Element, capacity),
		now:      time.Now,
	}
}

// NewWithTTL is like New plus a per-entry TTL. Entries past their
// TTL are treated as misses on Get and dropped on access.
func NewWithTTL[K comparable, V any](capacity int, ttl time.Duration) *Cache[K, V] {
	c := New[K, V](capacity)
	c.ttl = ttl
	return c
}

// Get returns the value for key + true on hit, zero value + false on
// miss (including TTL-expired entries).
func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return zero, false
	}
	e := el.Value.(*entry[K, V])
	if c.ttl > 0 && c.now().After(e.expiresAt) {
		c.removeElement(el)
		c.misses.Add(1)
		return zero, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return e.val, true
}

// Set stores key→val, evicting the least-recently-used entry when at
// capacity. Replacing an existing key resets its TTL.
func (c *Cache[K, V]) Set(key K, val V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[K, V])
		e.val = val
		if c.ttl > 0 {
			e.expiresAt = c.now().Add(c.ttl)
		}
		c.ll.MoveToFront(el)
		return
	}
	e := &entry[K, V]{key: key, val: val}
	if c.ttl > 0 {
		e.expiresAt = c.now().Add(c.ttl)
	}
	el := c.ll.PushFront(e)
	c.items[key] = el
	if c.ll.Len() > c.capacity {
		c.removeElement(c.ll.Back())
		c.evictions.Add(1)
	}
}

// Delete removes key. No-op when absent.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

// Len reports the live entry count (does NOT scan for TTL expiry —
// that happens lazily on Get).
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Stats returns a snapshot of the hit / miss / eviction counters.
func (c *Cache[K, V]) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}

func (c *Cache[K, V]) removeElement(el *list.Element) {
	e := el.Value.(*entry[K, V])
	c.ll.Remove(el)
	delete(c.items, e.key)
}
