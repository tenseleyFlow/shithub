// SPDX-License-Identifier: AGPL-3.0-or-later

package httpcache

import (
	"testing"
	"time"
)

func TestPageCache_GetSetRoundTrip(t *testing.T) {
	t.Parallel()
	c := NewPageCache(4, time.Minute)
	key := PageKey{RepoID: 1, BranchOID: "abc", Page: 1}
	body := []byte("<html>commits</html>")
	c.Set(key, body)
	got, ok := c.Get(key)
	if !ok {
		t.Fatalf("expected hit, got miss")
	}
	if string(got) != string(body) {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestPageCache_MissOnUnknownKey(t *testing.T) {
	t.Parallel()
	c := NewPageCache(4, time.Minute)
	if _, ok := c.Get(PageKey{RepoID: 1, BranchOID: "abc", Page: 1}); ok {
		t.Errorf("expected miss on empty cache")
	}
}

func TestPageCache_DistinctKeysIndependent(t *testing.T) {
	t.Parallel()
	c := NewPageCache(4, time.Minute)
	k1 := PageKey{RepoID: 1, BranchOID: "abc", Page: 1}
	k2 := PageKey{RepoID: 1, BranchOID: "abc", Page: 2}
	k3 := PageKey{RepoID: 2, BranchOID: "abc", Page: 1}
	k4 := PageKey{RepoID: 1, BranchOID: "xyz", Page: 1}
	c.Set(k1, []byte("a"))
	c.Set(k2, []byte("b"))
	c.Set(k3, []byte("c"))
	c.Set(k4, []byte("d"))
	for _, p := range []struct {
		k    PageKey
		want string
	}{{k1, "a"}, {k2, "b"}, {k3, "c"}, {k4, "d"}} {
		got, ok := c.Get(p.k)
		if !ok {
			t.Errorf("expected hit for %+v", p.k)
			continue
		}
		if string(got) != p.want {
			t.Errorf("key %+v: got %q, want %q", p.k, got, p.want)
		}
	}
}

func TestPageCache_EvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := NewPageCache(2, time.Minute)
	k1 := PageKey{RepoID: 1, BranchOID: "abc", Page: 1}
	k2 := PageKey{RepoID: 1, BranchOID: "abc", Page: 2}
	k3 := PageKey{RepoID: 1, BranchOID: "abc", Page: 3}
	c.Set(k1, []byte("1"))
	c.Set(k2, []byte("2"))
	c.Set(k3, []byte("3"))
	if _, ok := c.Get(k1); ok {
		t.Errorf("expected k1 to be evicted (cap=2, k1 was least recently used)")
	}
	if _, ok := c.Get(k2); !ok {
		t.Errorf("expected k2 to still be cached")
	}
	if _, ok := c.Get(k3); !ok {
		t.Errorf("expected k3 to still be cached")
	}
}

func TestPageCache_InvalidateBranch_DropsAllPages(t *testing.T) {
	t.Parallel()
	c := NewPageCache(8, time.Minute)
	k1 := PageKey{RepoID: 1, BranchOID: "old-oid", Page: 1}
	k2 := PageKey{RepoID: 1, BranchOID: "old-oid", Page: 2}
	k3 := PageKey{RepoID: 1, BranchOID: "old-oid", Page: 3}
	other := PageKey{RepoID: 1, BranchOID: "different-branch", Page: 1}
	otherRepo := PageKey{RepoID: 2, BranchOID: "old-oid", Page: 1}
	c.Set(k1, []byte("a"))
	c.Set(k2, []byte("b"))
	c.Set(k3, []byte("c"))
	c.Set(other, []byte("d"))
	c.Set(otherRepo, []byte("e"))

	c.InvalidateBranch(1, "old-oid")

	for _, k := range []PageKey{k1, k2, k3} {
		if _, ok := c.Get(k); ok {
			t.Errorf("expected %+v to be invalidated", k)
		}
	}
	if _, ok := c.Get(other); !ok {
		t.Errorf("InvalidateBranch must not drop different-branch entries")
	}
	if _, ok := c.Get(otherRepo); !ok {
		t.Errorf("InvalidateBranch must not drop entries from a different repo")
	}
}

func TestPageCache_InvalidateBranch_UnknownIsNoop(t *testing.T) {
	t.Parallel()
	c := NewPageCache(4, time.Minute)
	c.Set(PageKey{RepoID: 1, BranchOID: "abc", Page: 1}, []byte("x"))
	// Should NOT panic / NOT touch any unrelated key.
	c.InvalidateBranch(99, "nothing-here")
	if _, ok := c.Get(PageKey{RepoID: 1, BranchOID: "abc", Page: 1}); !ok {
		t.Errorf("unrelated invalidation must not drop the existing entry")
	}
}

func TestPageCache_TTLExpires(t *testing.T) {
	t.Parallel()
	c := NewPageCache(4, 5*time.Millisecond)
	key := PageKey{RepoID: 1, BranchOID: "abc", Page: 1}
	c.Set(key, []byte("body"))
	time.Sleep(20 * time.Millisecond)
	if _, ok := c.Get(key); ok {
		t.Errorf("expected TTL-expired miss after sleep")
	}
}

func TestPageCache_NilReceiver_AllNoops(t *testing.T) {
	t.Parallel()
	// The handler path passes a (possibly nil) cache through;
	// nil-receiver calls must be a clean no-op so tests and
	// degraded boot paths don't crash.
	var c *PageCache
	key := PageKey{RepoID: 1, BranchOID: "abc", Page: 1}
	if _, ok := c.Get(key); ok {
		t.Errorf("nil receiver Get must miss")
	}
	c.Set(key, []byte("ignored")) // must not panic
	c.InvalidateBranch(1, "abc")  // must not panic
	if got := c.Stats(); got.Hits != 0 || got.Misses != 0 || got.Evictions != 0 {
		t.Errorf("nil receiver Stats must be zero, got %+v", got)
	}
}

func TestNewPageCache_RejectsZeroOrNegativeInputs(t *testing.T) {
	t.Parallel()
	if got := NewPageCache(0, time.Minute); got != nil {
		t.Errorf("capacity 0 should return nil; got %+v", got)
	}
	if got := NewPageCache(-1, time.Minute); got != nil {
		t.Errorf("negative capacity should return nil; got %+v", got)
	}
	if got := NewPageCache(4, 0); got != nil {
		t.Errorf("ttl 0 should return nil; got %+v", got)
	}
}

func TestPageCache_StatsTrackHitsAndMisses(t *testing.T) {
	t.Parallel()
	c := NewPageCache(4, time.Minute)
	key := PageKey{RepoID: 1, BranchOID: "abc", Page: 1}
	c.Set(key, []byte("x"))
	_, _ = c.Get(key)                                         // hit
	_, _ = c.Get(key)                                         // hit
	_, _ = c.Get(PageKey{RepoID: 9, BranchOID: "?", Page: 1}) // miss
	s := c.Stats()
	if s.Hits != 2 {
		t.Errorf("Hits = %d, want 2", s.Hits)
	}
	if s.Misses != 1 {
		t.Errorf("Misses = %d, want 1", s.Misses)
	}
}
