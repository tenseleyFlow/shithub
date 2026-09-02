// SPDX-License-Identifier: AGPL-3.0-or-later

package treecache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func commitMap(subject string) map[string]gitops.Commit {
	return map[string]gitops.Commit{"README.md": {Subject: subject}}
}

// countingFetch returns a fetch func plus a pointer to its call count.
func countingFetch(subject string) (func(context.Context) (map[string]gitops.Commit, error), *int) {
	calls := 0
	return func(context.Context) (map[string]gitops.Commit, error) {
		calls++
		return commitMap(subject), nil
	}, &calls
}

func TestLastCommits_SecondCallIsAHit(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	key := EntryKey{RepoID: 1, CommitOID: "aaa", Subpath: ""}
	fetch, calls := countingFetch("first")

	for i := 0; i < 3; i++ {
		got, err := c.LastCommits(context.Background(), key, fetch)
		if err != nil {
			t.Fatalf("LastCommits: %v", err)
		}
		if got["README.md"].Subject != "first" {
			t.Fatalf("subject = %q, want %q", got["README.md"].Subject, "first")
		}
	}
	if *calls != 1 {
		t.Errorf("fetch called %d times, want 1", *calls)
	}
	// lru.Group re-checks the cache after winning the single-flight
	// slot, so one cold key records two misses.
	if s := c.Stats(); s.Hits != 2 || s.Misses != 2 {
		t.Errorf("stats = %+v, want 2 hits / 2 misses", s)
	}
}

func TestLastCommits_NewCommitOIDMissesAndRefetches(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	oldKey := EntryKey{RepoID: 1, CommitOID: "old-oid"}
	newKey := EntryKey{RepoID: 1, CommitOID: "new-oid"}

	oldFetch, oldCalls := countingFetch("before push")
	if _, err := c.LastCommits(context.Background(), oldKey, oldFetch); err != nil {
		t.Fatalf("LastCommits(old): %v", err)
	}
	// A push lands: the ref now points at a different commit, so the
	// key changes and the cache must not serve the pre-push value.
	newFetch, newCalls := countingFetch("after push")
	got, err := c.LastCommits(context.Background(), newKey, newFetch)
	if err != nil {
		t.Fatalf("LastCommits(new): %v", err)
	}
	if got["README.md"].Subject != "after push" {
		t.Errorf("subject = %q, want %q — stale entry served across an OID change",
			got["README.md"].Subject, "after push")
	}
	if *oldCalls != 1 || *newCalls != 1 {
		t.Errorf("fetch calls: old=%d new=%d, want 1 and 1", *oldCalls, *newCalls)
	}
}

func TestLastCommits_KeyComponentsAreIndependent(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	base := EntryKey{RepoID: 1, CommitOID: "oid", Subpath: "src"}
	for _, k := range []EntryKey{
		base,
		{RepoID: 2, CommitOID: "oid", Subpath: "src"},
		{RepoID: 1, CommitOID: "other", Subpath: "src"},
		{RepoID: 1, CommitOID: "oid", Subpath: "docs"},
	} {
		fetch, calls := countingFetch("x")
		if _, err := c.LastCommits(context.Background(), k, fetch); err != nil {
			t.Fatalf("LastCommits(%+v): %v", k, err)
		}
		if *calls != 1 {
			t.Errorf("key %+v collided with an earlier key (fetch calls = %d)", k, *calls)
		}
	}
}

func TestLastCommits_EmptyOIDBypassesTheCache(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	key := EntryKey{RepoID: 1, CommitOID: ""}
	fetch, calls := countingFetch("x")
	for i := 0; i < 3; i++ {
		if _, err := c.LastCommits(context.Background(), key, fetch); err != nil {
			t.Fatalf("LastCommits: %v", err)
		}
	}
	if *calls != 3 {
		t.Errorf("fetch called %d times, want 3 (an unresolved ref must not be cached)", *calls)
	}
}

func TestLastCommits_TTLExpires(t *testing.T) {
	t.Parallel()
	c := New(8, 5*time.Millisecond)
	key := EntryKey{RepoID: 1, CommitOID: "aaa"}
	fetch, calls := countingFetch("x")
	if _, err := c.LastCommits(context.Background(), key, fetch); err != nil {
		t.Fatalf("LastCommits: %v", err)
	}
	time.Sleep(25 * time.Millisecond)
	if _, err := c.LastCommits(context.Background(), key, fetch); err != nil {
		t.Fatalf("LastCommits: %v", err)
	}
	if *calls != 2 {
		t.Errorf("fetch called %d times, want 2 (TTL should have expired the entry)", *calls)
	}
}

func TestLastCommits_EvictsLeastRecentlyUsed(t *testing.T) {
	t.Parallel()
	c := New(2, time.Minute)
	k1 := EntryKey{RepoID: 1, CommitOID: "a"}
	k2 := EntryKey{RepoID: 1, CommitOID: "b"}
	k3 := EntryKey{RepoID: 1, CommitOID: "c"}
	f1, c1 := countingFetch("1")
	f2, c2 := countingFetch("2")
	f3, c3 := countingFetch("3")
	ctx := context.Background()
	mustFetch := func(k EntryKey, f func(context.Context) (map[string]gitops.Commit, error)) {
		t.Helper()
		if _, err := c.LastCommits(ctx, k, f); err != nil {
			t.Fatalf("LastCommits(%+v): %v", k, err)
		}
	}
	mustFetch(k1, f1)
	mustFetch(k2, f2)
	mustFetch(k3, f3)
	mustFetch(k1, f1) // k1 was evicted → refetch
	mustFetch(k3, f3) // k3 is still resident → hit
	if *c1 != 2 {
		t.Errorf("k1 fetches = %d, want 2 (should have been evicted)", *c1)
	}
	if *c2 != 1 || *c3 != 1 {
		t.Errorf("k2/k3 fetches = %d/%d, want 1/1", *c2, *c3)
	}
}

func TestLastCommits_ErrorsAreNotCached(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	key := EntryKey{RepoID: 1, CommitOID: "aaa"}
	boom := errors.New("git exploded")
	calls := 0
	fetch := func(context.Context) (map[string]gitops.Commit, error) {
		calls++
		if calls == 1 {
			return nil, boom
		}
		return commitMap("recovered"), nil
	}
	if _, err := c.LastCommits(context.Background(), key, fetch); !errors.Is(err, boom) {
		t.Fatalf("first call error = %v, want %v", err, boom)
	}
	got, err := c.LastCommits(context.Background(), key, fetch)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got["README.md"].Subject != "recovered" {
		t.Errorf("subject = %q; a transient failure must not poison the key", got["README.md"].Subject)
	}
}

func TestLastCommits_ConcurrentMissesCollapseToOneFetch(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	key := EntryKey{RepoID: 1, CommitOID: "aaa"}
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	fetch := func(context.Context) (map[string]gitops.Commit, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return commitMap("x"), nil
	}

	const racers = 16
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.LastCommits(context.Background(), key, fetch)
		}()
	}
	// Give the racers time to pile onto the single-flight slot, then
	// let the one real fetch finish.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("fetch called %d times under %d concurrent misses, want 1", calls, racers)
	}
}

func TestCommitCount_CachesPerRevision(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	calls := 0
	fetch := func(context.Context) (int, error) { calls++; return 42, nil }
	key := RevKey{RepoID: 1, CommitOID: "aaa"}
	for i := 0; i < 3; i++ {
		got, err := c.CommitCount(context.Background(), key, fetch)
		if err != nil {
			t.Fatalf("CommitCount: %v", err)
		}
		if got != 42 {
			t.Fatalf("count = %d, want 42", got)
		}
	}
	if calls != 1 {
		t.Errorf("fetch called %d times, want 1", calls)
	}
	// New head OID → new key → refetch.
	if _, err := c.CommitCount(context.Background(), RevKey{RepoID: 1, CommitOID: "bbb"}, fetch); err != nil {
		t.Fatalf("CommitCount(new oid): %v", err)
	}
	if calls != 2 {
		t.Errorf("fetch called %d times after an OID change, want 2", calls)
	}
}

func TestLanguagesAndContributors_CacheAndInvalidateOnOIDChange(t *testing.T) {
	t.Parallel()
	c := New(8, time.Minute)
	ctx := context.Background()

	langCalls := 0
	langFetch := func(context.Context) (map[string]int64, error) {
		langCalls++
		return map[string]int64{"Go": 100}, nil
	}
	contribCalls := 0
	contribFetch := func(context.Context) ([]ContributorTally, error) {
		contribCalls++
		return []ContributorTally{{Name: "A", Email: "a@e", Count: 3}}, nil
	}

	old := RevKey{RepoID: 7, CommitOID: "old"}
	fresh := RevKey{RepoID: 7, CommitOID: "new"}
	for i := 0; i < 2; i++ {
		if _, err := c.Languages(ctx, old, langFetch); err != nil {
			t.Fatalf("Languages: %v", err)
		}
		if _, err := c.Contributors(ctx, old, contribFetch); err != nil {
			t.Fatalf("Contributors: %v", err)
		}
	}
	if langCalls != 1 || contribCalls != 1 {
		t.Fatalf("cold fetches: lang=%d contrib=%d, want 1 and 1", langCalls, contribCalls)
	}
	if _, err := c.Languages(ctx, fresh, langFetch); err != nil {
		t.Fatalf("Languages(new): %v", err)
	}
	if _, err := c.Contributors(ctx, fresh, contribFetch); err != nil {
		t.Fatalf("Contributors(new): %v", err)
	}
	if langCalls != 2 || contribCalls != 2 {
		t.Errorf("after OID change: lang=%d contrib=%d, want 2 and 2", langCalls, contribCalls)
	}
}

func TestNew_RejectsZeroOrNegativeInputs(t *testing.T) {
	t.Parallel()
	if got := New(0, time.Minute); got != nil {
		t.Errorf("capacity 0 should return nil, got %+v", got)
	}
	if got := New(-1, time.Minute); got != nil {
		t.Errorf("negative capacity should return nil, got %+v", got)
	}
	if got := New(8, 0); got != nil {
		t.Errorf("ttl 0 should return nil, got %+v", got)
	}
}

func TestNew_TinyCapacityStillBuildsHeavyCaches(t *testing.T) {
	t.Parallel()
	// capacity/heavyCapRatio floors at 1 rather than panicking the
	// underlying LRU with a zero capacity.
	c := New(1, time.Minute)
	if c == nil {
		t.Fatal("New(1, ttl) = nil")
	}
	if _, err := c.Languages(context.Background(), RevKey{RepoID: 1, CommitOID: "a"},
		func(context.Context) (map[string]int64, error) { return nil, nil }); err != nil {
		t.Fatalf("Languages: %v", err)
	}
}

func TestNilCache_AlwaysFetchesAndReportsZeroStats(t *testing.T) {
	t.Parallel()
	var c *Cache
	ctx := context.Background()
	calls := 0
	if _, err := c.LastCommits(ctx, EntryKey{RepoID: 1, CommitOID: "a"},
		func(context.Context) (map[string]gitops.Commit, error) { calls++; return nil, nil }); err != nil {
		t.Fatalf("LastCommits: %v", err)
	}
	if _, err := c.CommitCount(ctx, RevKey{RepoID: 1, CommitOID: "a"},
		func(context.Context) (int, error) { calls++; return 0, nil }); err != nil {
		t.Fatalf("CommitCount: %v", err)
	}
	if _, err := c.Languages(ctx, RevKey{RepoID: 1, CommitOID: "a"},
		func(context.Context) (map[string]int64, error) { calls++; return nil, nil }); err != nil {
		t.Fatalf("Languages: %v", err)
	}
	if _, err := c.Contributors(ctx, RevKey{RepoID: 1, CommitOID: "a"},
		func(context.Context) ([]ContributorTally, error) { calls++; return nil, nil }); err != nil {
		t.Fatalf("Contributors: %v", err)
	}
	if calls != 4 {
		t.Errorf("nil cache made %d fetches, want 4", calls)
	}
	if got := c.Stats(); got.Hits != 0 || got.Misses != 0 || got.Evictions != 0 {
		t.Errorf("nil-receiver Stats = %+v, want zero", got)
	}
}
