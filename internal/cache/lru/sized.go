// SPDX-License-Identifier: AGPL-3.0-or-later

package lru

import (
	"container/list"
	"sync"
	"sync/atomic"
)

// SizedCache is a byte-bounded LRU. Each entry contributes a caller-
// supplied size; eviction runs while the running total exceeds the
// cap. Use this for caches whose values vary widely in memory cost
// (rendered HTML, diff blobs, response bodies).
type SizedCache[K comparable] struct {
	mu       sync.Mutex
	maxBytes int64
	cur      int64
	ll       *list.List
	items    map[K]*list.Element

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

type sizedEntry[K comparable] struct {
	key  K
	val  []byte
	size int64
}

// NewSized constructs a byte-bounded LRU. maxBytes must be positive.
func NewSized[K comparable](maxBytes int64) *SizedCache[K] {
	if maxBytes <= 0 {
		panic("lru: maxBytes must be positive")
	}
	return &SizedCache[K]{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[K]*list.Element),
	}
}

// Get returns the cached bytes + true on hit. The returned slice is
// the cached buffer (zero-copy) — callers MUST NOT mutate it. Use
// `append([]byte(nil), v...)` if mutation is needed.
func (c *SizedCache[K]) Get(key K) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return el.Value.(*sizedEntry[K]).val, true
}

// Set stores key→val. Replacing an existing key updates its size.
// Eviction runs after insertion until total bytes ≤ maxBytes.
func (c *SizedCache[K]) Set(key K, val []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := int64(len(val))
	if el, ok := c.items[key]; ok {
		e := el.Value.(*sizedEntry[K])
		c.cur += size - e.size
		e.val = val
		e.size = size
		c.ll.MoveToFront(el)
	} else {
		e := &sizedEntry[K]{key: key, val: val, size: size}
		el := c.ll.PushFront(e)
		c.items[key] = el
		c.cur += size
	}
	for c.cur > c.maxBytes && c.ll.Len() > 1 {
		c.removeElement(c.ll.Back())
		c.evictions.Add(1)
	}
}

// Delete removes key. No-op when absent.
func (c *SizedCache[K]) Delete(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
	}
}

// Bytes reports the current total payload size.
func (c *SizedCache[K]) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// Len reports the entry count.
func (c *SizedCache[K]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Stats returns a snapshot of the hit / miss / eviction counters.
func (c *SizedCache[K]) Stats() Stats {
	return Stats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
	}
}

func (c *SizedCache[K]) removeElement(el *list.Element) {
	e := el.Value.(*sizedEntry[K])
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.cur -= e.size
}
