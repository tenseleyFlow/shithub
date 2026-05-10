// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"context"
	"testing"

	srch "github.com/tenseleyFlow/shithub/internal/search"
)

// TestCanonicalizeQuery_StableAcrossWhitespaceAndCase pins the cache
// key contract: equivalent ParsedQuery values produce equal cache
// keys regardless of original whitespace or letter case. Without
// this, the cache hit-rate would collapse on common query variants.
func TestCanonicalizeQuery_StableAcrossWhitespaceAndCase(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b srch.ParsedQuery
	}{
		{
			"casing",
			srch.ParsedQuery{Text: "FooBar"},
			srch.ParsedQuery{Text: "foobar"},
		},
		{
			"whitespace",
			srch.ParsedQuery{Text: "foo  bar"},
			srch.ParsedQuery{Text: " foo bar "},
		},
		{
			"phrase casing",
			srch.ParsedQuery{Text: "x", Phrase: "Hello World"},
			srch.ParsedQuery{Text: "x", Phrase: "hello world"},
		},
		{
			"repo filter casing",
			srch.ParsedQuery{Text: "x", RepoFilter: &srch.RepoFilter{Owner: "Alice", Name: "Repo"}},
			srch.ParsedQuery{Text: "x", RepoFilter: &srch.RepoFilter{Owner: "alice", Name: "repo"}},
		},
		{
			"state casing",
			srch.ParsedQuery{Text: "x", StateFilter: "OPEN"},
			srch.ParsedQuery{Text: "x", StateFilter: "open"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ka := canonicalizeQuery(tc.a)
			kb := canonicalizeQuery(tc.b)
			if ka != kb {
				t.Fatalf("canonicalizeQuery diverged:\n a=%q\n b=%q", ka, kb)
			}
		})
	}
}

// TestCanonicalizeQuery_DistinctFiltersDistinct pins that DIFFERENT
// queries don't collide. Same Text but different filters MUST produce
// different keys — otherwise a viewer with no access to a private
// `repo:secret` could see its result count via cache pollution from
// a separate query.
func TestCanonicalizeQuery_DistinctFiltersDistinct(t *testing.T) {
	t.Parallel()

	base := srch.ParsedQuery{Text: "foo"}
	variants := []srch.ParsedQuery{
		{Text: "foo", Phrase: "exact"},
		{Text: "foo", RepoFilter: &srch.RepoFilter{Owner: "a", Name: "b"}},
		{Text: "foo", StateFilter: "open"},
		{Text: "foo", AuthorFilter: "alice"},
	}
	baseKey := canonicalizeQuery(base)
	for i, v := range variants {
		got := canonicalizeQuery(v)
		if got == baseKey {
			t.Errorf("variant %d collides with base: %q", i, got)
		}
	}
}

// TestTabsCache_HitOnSameKey pins the cache contract:
// repeated lookups for the same (query, viewer) within the TTL hit
// the cache and the fetcher runs at most once.
func TestTabsCache_HitOnSameKey(t *testing.T) {
	t.Parallel()

	cache := newTabsCache()
	key := tabsCacheKey{q: "t=foo", userID: 7}

	calls := 0
	want := []searchTab{{Key: "code", Count: 42}}
	for i := 0; i < 5; i++ {
		got, err := cache.g.Do(context.Background(), key, func(_ context.Context) ([]searchTab, error) {
			calls++
			return want, nil
		})
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		if len(got) != 1 || got[0].Count != 42 {
			t.Fatalf("got = %+v", got)
		}
	}
	if calls != 1 {
		t.Fatalf("fetcher invoked %d times; want 1", calls)
	}
}

// TestTabsCache_DistinctKeysIsolated pins the per-viewer isolation
// invariant: Alice's count must not leak to Bob's render even when
// the canonicalized query matches.
func TestTabsCache_DistinctKeysIsolated(t *testing.T) {
	t.Parallel()

	cache := newTabsCache()
	alice := tabsCacheKey{q: "t=foo", userID: 1}
	bob := tabsCacheKey{q: "t=foo", userID: 2}

	a, _ := cache.g.Do(context.Background(), alice, func(_ context.Context) ([]searchTab, error) {
		return []searchTab{{Key: "repositories", Count: 10}}, nil
	})
	b, _ := cache.g.Do(context.Background(), bob, func(_ context.Context) ([]searchTab, error) {
		return []searchTab{{Key: "repositories", Count: 99}}, nil
	})
	if a[0].Count == b[0].Count {
		t.Fatalf("alice and bob got the same count — visibility leak")
	}
}
