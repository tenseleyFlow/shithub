// SPDX-License-Identifier: AGPL-3.0-or-later

package lru

import (
	"context"

	"golang.org/x/sync/singleflight"
)

// Group wraps a Cache with single-flight semantics so a hot-key
// miss doesn't spawn N concurrent upstream calls. The fetch function
// is invoked at most once per (key, in-flight wave) — concurrent
// callers wait on the same goroutine and receive its result.
//
// Use this whenever the upstream is non-trivial: a `git rev-list`
// subprocess, an FS walk, a multi-row DB read.
type Group[K comparable, V any] struct {
	cache *Cache[K, V]
	sf    singleflight.Group
	// keyer converts the typed key into the singleflight string key.
	// We keep the cache strongly-typed but singleflight is string-
	// keyed, so callers supply a stable string mapping.
	keyer func(K) string
}

// NewGroup wraps cache with singleflight. keyer must produce a
// stable, unique string for every distinct K (default: `fmt.Sprint`-
// equivalent for the type). For composite keys (struct), the caller
// is on the hook for serialization.
func NewGroup[K comparable, V any](cache *Cache[K, V], keyer func(K) string) *Group[K, V] {
	if cache == nil {
		panic("lru: nil Cache in NewGroup")
	}
	if keyer == nil {
		panic("lru: nil keyer in NewGroup")
	}
	return &Group[K, V]{cache: cache, keyer: keyer}
}

// Do returns the cached value when present, otherwise invokes fetch
// (single-flighted) and caches the result before returning.
//
// Errors from fetch are NOT cached — a transient failure on key K
// shouldn't poison subsequent reads. Callers that want negative-
// caching add their own sentinel value.
func (g *Group[K, V]) Do(ctx context.Context, key K, fetch func(ctx context.Context) (V, error)) (V, error) {
	if v, ok := g.cache.Get(key); ok {
		return v, nil
	}
	sk := g.keyer(key)
	v, err, _ := g.sf.Do(sk, func() (any, error) {
		// Re-check the cache after acquiring the singleflight slot:
		// the previous in-flight call may have populated it while we
		// were waiting.
		if v, ok := g.cache.Get(key); ok {
			return v, nil
		}
		v, err := fetch(ctx)
		if err != nil {
			return v, err
		}
		g.cache.Set(key, v)
		return v, nil
	})
	if err != nil {
		var zero V
		return zero, err
	}
	return v.(V), nil
}

// Invalidate drops key from the cache. Safe to call from anywhere
// (push handlers, settings updates) without coordinating with
// in-flight singleflight callers — the next Do re-fetches.
func (g *Group[K, V]) Invalidate(key K) { g.cache.Delete(key) }

// Stats reports the underlying cache's counters.
func (g *Group[K, V]) Stats() Stats { return g.cache.Stats() }
