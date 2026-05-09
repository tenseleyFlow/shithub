// SPDX-License-Identifier: AGPL-3.0-or-later

package lru

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_GetSetEviction(t *testing.T) {
	t.Parallel()
	c := New[string, int](2)
	c.Set("a", 1)
	c.Set("b", 2)
	if v, ok := c.Get("a"); !ok || v != 1 {
		t.Fatalf("Get(a) = %d,%v; want 1,true", v, ok)
	}
	// Touching "a" makes "b" the LRU.
	c.Set("c", 3)
	if _, ok := c.Get("b"); ok {
		t.Errorf("b should have been evicted")
	}
	if v, ok := c.Get("c"); !ok || v != 3 {
		t.Errorf("c = %d,%v; want 3,true", v, ok)
	}
	if s := c.Stats(); s.Evictions != 1 {
		t.Errorf("Evictions = %d; want 1", s.Evictions)
	}
}

func TestCache_TTLExpiry(t *testing.T) {
	t.Parallel()
	c := NewWithTTL[string, int](4, 50*time.Millisecond)
	now := time.Now()
	c.now = func() time.Time { return now }
	c.Set("k", 42)
	if v, ok := c.Get("k"); !ok || v != 42 {
		t.Fatalf("fresh hit: got %d,%v; want 42,true", v, ok)
	}
	now = now.Add(60 * time.Millisecond)
	if _, ok := c.Get("k"); ok {
		t.Errorf("expired entry should be a miss")
	}
}

func TestCache_DeleteAndStats(t *testing.T) {
	t.Parallel()
	c := New[string, int](2)
	c.Set("a", 1)
	c.Get("a")
	c.Get("missing")
	c.Delete("a")
	if c.Len() != 0 {
		t.Errorf("Len after Delete = %d; want 0", c.Len())
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 1 {
		t.Errorf("stats = %+v; want hits=1 misses=1", s)
	}
}

func TestSizedCache_BytesBounded(t *testing.T) {
	t.Parallel()
	c := NewSized[string](100)
	c.Set("a", make([]byte, 60))
	c.Set("b", make([]byte, 60)) // forces eviction of "a"
	if _, ok := c.Get("a"); ok {
		t.Errorf("a should have been evicted to fit b")
	}
	if c.Bytes() != 60 {
		t.Errorf("Bytes = %d; want 60", c.Bytes())
	}
}

func TestSizedCache_ReplaceShrinks(t *testing.T) {
	t.Parallel()
	c := NewSized[string](100)
	c.Set("a", make([]byte, 80))
	c.Set("a", make([]byte, 10)) // smaller replacement
	if c.Bytes() != 10 {
		t.Errorf("Bytes after shrink = %d; want 10", c.Bytes())
	}
}

func TestGroup_SingleFlightCollapsesConcurrentMisses(t *testing.T) {
	t.Parallel()
	c := New[string, int](16)
	g := NewGroup(c, func(s string) string { return s })

	var calls atomic.Int64
	fetch := func(ctx context.Context) (int, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return 99, nil
	}

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			v, err := g.Do(context.Background(), "k", fetch)
			if err != nil || v != 99 {
				t.Errorf("Do = %d,%v; want 99,nil", v, err)
			}
		}()
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("upstream called %d times; want 1 (singleflight collapse failed)", calls.Load())
	}
}

func TestGroup_ErrorNotCached(t *testing.T) {
	t.Parallel()
	c := New[string, int](4)
	g := NewGroup(c, func(s string) string { return s })

	var attempt atomic.Int64
	fetch := func(ctx context.Context) (int, error) {
		n := attempt.Add(1)
		if n == 1 {
			return 0, errors.New("transient")
		}
		return 7, nil
	}
	if _, err := g.Do(context.Background(), "k", fetch); err == nil {
		t.Fatalf("expected error on first call")
	}
	v, err := g.Do(context.Background(), "k", fetch)
	if err != nil {
		t.Fatalf("second call err: %v", err)
	}
	if v != 7 {
		t.Errorf("v = %d; want 7", v)
	}
}

func BenchmarkCacheSetGet(b *testing.B) {
	c := New[int, int](1024)
	for i := 0; i < 1024; i++ {
		c.Set(i, i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = c.Get(i & 1023)
	}
}

// Reference for keyer construction in the test above.
var _ = strconv.Itoa
