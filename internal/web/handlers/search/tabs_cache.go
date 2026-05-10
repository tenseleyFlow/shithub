// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"fmt"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/cache/lru"
	srch "github.com/tenseleyFlow/shithub/internal/search"
)

// tabsCacheKey is the (canonical-query, viewer-id, anon) tuple every
// distinct count slice maps to. Anonymous viewers share one cache
// slot per query (their visibility is the same: public-only). Authed
// viewers each get their own bucket because what they can read
// differs (private repos they collaborate on, etc.) and the tab
// counts must reflect that — sharing the slice across viewers would
// leak the existence of private results.
//
// We canonicalize the query with strings.ToLower + collapse-spaces
// so q="foo" and q="FOO " hit the same slot. Operators (repo:, is:,
// state:, author:) are folded into the canonical form by
// canonicalizeQuery's ParsedQuery round-trip — same parsed shape
// produces the same cache key.
type tabsCacheKey struct {
	q      string // canonical query string
	userID int64  // 0 for anonymous
}

// tabsCacheTTL is short enough that stale counts can't mislead an
// operator triaging a recent push, long enough to absorb the
// dashboard-style "user types in search box, browser auto-fires
// repeatedly" pattern.
const (
	tabsCacheTTL  = 30 * time.Second
	tabsCacheSize = 1024
)

// tabsCache wraps a small LRU around the per-(query, viewer) tab-
// count slice the searchTabs renderer needs. Pre-fix the renderer
// fired 5 FTS counts on EVERY GET /search render — six queries per
// page since the active tab also runs its own search. With this
// cache, the steady-state cost of a hot query is a single lookup
// (assuming the active tab still runs since its result list is
// not cached, only the 5 count-only calls are).
//
// Single-flighted via lru.Group so a thundering-herd on the same
// (q, viewer) coalesces into one upstream wave.
type tabsCache struct {
	g *lru.Group[tabsCacheKey, []searchTab]
}

func newTabsCache() *tabsCache {
	c := lru.NewWithTTL[tabsCacheKey, []searchTab](tabsCacheSize, tabsCacheTTL)
	g := lru.NewGroup(c, func(k tabsCacheKey) string {
		return fmt.Sprintf("%d|%s", k.userID, k.q)
	})
	return &tabsCache{g: g}
}

// canonicalizeQuery returns a stable string key for ParsedQuery.
// Two raw queries that parse identically produce the same key.
func canonicalizeQuery(p srch.ParsedQuery) string {
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strings.ToLower(strings.Join(strings.Fields(p.Text), " ")))
	if p.Phrase != "" {
		b.WriteString("|p=")
		b.WriteString(strings.ToLower(p.Phrase))
	}
	if p.RepoFilter != nil {
		b.WriteString("|r=")
		b.WriteString(strings.ToLower(p.RepoFilter.Owner))
		b.WriteString("/")
		b.WriteString(strings.ToLower(p.RepoFilter.Name))
	}
	if p.StateFilter != "" {
		b.WriteString("|s=")
		b.WriteString(strings.ToLower(p.StateFilter))
	}
	if p.AuthorFilter != "" {
		b.WriteString("|a=")
		b.WriteString(strings.ToLower(p.AuthorFilter))
	}
	return b.String()
}
