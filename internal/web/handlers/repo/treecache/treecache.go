// SPDX-License-Identifier: AGPL-3.0-or-later

// Package treecache is the in-process cache behind the repo home /
// code tab, the most-crawled page on the site.
//
// Rendering one directory listing used to cost, per request and with
// no caching at all: one `git log -1` per tree entry, one
// `git rev-list --count`, one `git log -n 500` for the contributor
// strip, and one recursive `git ls-tree -r` for the language bar. A
// crawler walking a 100-entry directory therefore forked git ~104
// times per page view, anonymously.
//
// Every value here is keyed by the *commit OID* being rendered, so
// invalidation is structural: a push moves the ref, the OID changes,
// and the new key misses cleanly while the old entries age out via
// TTL and LRU eviction. There is no invalidation hook to forget to
// call — the only staleness window is a mutation that does NOT change
// the commit OID, which for these four values cannot happen.
//
// Construction follows the httpcache.PageCache pattern: exactly one
// Cache per process, built in internal/web/repo_wiring.go and passed
// through repo.Deps. Every method is nil-receiver safe so tests and
// degraded boot paths can leave it unwired and simply always miss.
package treecache

import (
	"context"
	"strconv"
	"time"

	"github.com/tenseleyFlow/shithub/internal/cache/lru"
	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// EntryKey identifies one directory listing's last-commit column:
// the repo, the commit being rendered, and the subpath inside it
// ("" for the repo root).
type EntryKey struct {
	RepoID    int64
	CommitOID string
	Subpath   string
}

// RevKey identifies whole-revision data — anything derived from the
// commit as a whole rather than from one directory in it.
type RevKey struct {
	RepoID    int64
	CommitOID string
}

// ContributorTally is one author's commit count within the bounded
// contributor walk. Caching the tally rather than the raw commit list
// keeps the entry proportional to the number of distinct authors
// (single digits for most repos) instead of to the 500-commit window,
// and leaves identity resolution — which reads the users table — on
// the per-request path where it belongs.
type ContributorTally struct {
	Name  string
	Email string
	Count int
}

// Sizes are deliberately modest: this process shares a 3.9 GB box
// with Postgres and has a 1200 MiB GOMEMLIMIT, and the whole point of
// the availability campaign is to stop it from being the largest
// thing on the machine.
//
//   - LastCommits: one entry per (repo, commit, directory). The value
//     is a map of basename → Commit with no body, ≈150 B per row, so a
//     typical 10-entry directory is ~1.5 KB and a 200-entry directory
//     is ~30 KB. 2,048 entries is a few MB in practice and ~60 MB in
//     an adversarial worst case that no real repo set produces.
//   - CommitCounts: an int. 2,048 entries is noise.
//   - Languages: a name → bytes map, ≤ ~20 rows after aggregation.
//   - Contributors: one row per distinct author in the last 500
//     commits. In theory unbounded (500 commits could be 500 distinct
//     authors), so this and Languages get capacity/4 slots.
//
// The 10-minute TTL is a backstop, not the correctness mechanism —
// the OID in the key is. It exists so a repo that stops being visited
// releases its memory instead of squatting an LRU slot until eviction
// pressure arrives.
const (
	// DefaultCapacity bounds the two cheap caches; the heavier
	// language and contributor caches get DefaultCapacity/heavyCapRatio.
	DefaultCapacity = 2048
	// DefaultTTL is the memory-release backstop, not the correctness
	// mechanism — the commit OID in the key is.
	DefaultTTL = 10 * time.Minute

	heavyCapRatio = 4
	minCapacity   = 1
)

// Cache bundles the four code-tab caches. Each is an lru.Group, so a
// burst of concurrent misses on the same hot key (exactly what a
// crawler produces) collapses into one git invocation.
type Cache struct {
	lastCommits  *lru.Group[EntryKey, map[string]gitops.Commit]
	commitCounts *lru.Group[RevKey, int]
	languages    *lru.Group[RevKey, map[string]int64]
	contributors *lru.Group[RevKey, []ContributorTally]
}

// New builds the process-wide cache. capacity bounds the two cheap
// caches; the heavier language and contributor caches get
// capacity/heavyCapRatio, floored at 1. A non-positive capacity or
// TTL returns nil, which every method treats as "always miss".
func New(capacity int, ttl time.Duration) *Cache {
	if capacity <= 0 || ttl <= 0 {
		return nil
	}
	heavy := capacity / heavyCapRatio
	if heavy < minCapacity {
		heavy = minCapacity
	}
	return &Cache{
		lastCommits: lru.NewGroup(
			lru.NewWithTTL[EntryKey, map[string]gitops.Commit](capacity, ttl), entryKeyString),
		commitCounts: lru.NewGroup(
			lru.NewWithTTL[RevKey, int](capacity, ttl), revKeyString),
		languages: lru.NewGroup(
			lru.NewWithTTL[RevKey, map[string]int64](heavy, ttl), revKeyString),
		contributors: lru.NewGroup(
			lru.NewWithTTL[RevKey, []ContributorTally](heavy, ttl), revKeyString),
	}
}

func entryKeyString(k EntryKey) string {
	return strconv.FormatInt(k.RepoID, 10) + "|" + k.CommitOID + "|" + k.Subpath
}

func revKeyString(k RevKey) string {
	return strconv.FormatInt(k.RepoID, 10) + "|" + k.CommitOID
}

// LastCommits returns the cached last-commit-per-entry map for key,
// invoking fetch on a miss. Keys with an empty CommitOID bypass the
// cache entirely — an unresolvable ref has no stable identity to key
// on, and caching under "" would collide across revisions.
func (c *Cache) LastCommits(
	ctx context.Context, key EntryKey,
	fetch func(context.Context) (map[string]gitops.Commit, error),
) (map[string]gitops.Commit, error) {
	if c == nil || key.CommitOID == "" {
		return fetch(ctx)
	}
	return c.lastCommits.Do(ctx, key, fetch)
}

// CommitCount returns the cached `rev-list --count` for key.
func (c *Cache) CommitCount(
	ctx context.Context, key RevKey, fetch func(context.Context) (int, error),
) (int, error) {
	if c == nil || key.CommitOID == "" {
		return fetch(ctx)
	}
	return c.commitCounts.Do(ctx, key, fetch)
}

// Languages returns the cached language → bytes aggregate for key.
// This is the `ls-tree -r`-derived value: the recursive walk itself is
// never cached, only the handful of numbers it reduces to.
func (c *Cache) Languages(
	ctx context.Context, key RevKey, fetch func(context.Context) (map[string]int64, error),
) (map[string]int64, error) {
	if c == nil || key.CommitOID == "" {
		return fetch(ctx)
	}
	return c.languages.Do(ctx, key, fetch)
}

// Contributors returns the cached author tally for key — the reduced
// form of the bounded `git log -n 500` the About sidebar runs.
func (c *Cache) Contributors(
	ctx context.Context, key RevKey, fetch func(context.Context) ([]ContributorTally, error),
) ([]ContributorTally, error) {
	if c == nil || key.CommitOID == "" {
		return fetch(ctx)
	}
	return c.contributors.Do(ctx, key, fetch)
}

// Stats reports the four caches' hit/miss/eviction counters, summed.
// Nil receiver returns the zero value so callers don't have to branch.
func (c *Cache) Stats() lru.Stats {
	if c == nil {
		return lru.Stats{}
	}
	var out lru.Stats
	for _, s := range []lru.Stats{
		c.lastCommits.Stats(), c.commitCounts.Stats(),
		c.languages.Stats(), c.contributors.Stats(),
	} {
		out.Hits += s.Hits
		out.Misses += s.Misses
		out.Evictions += s.Evictions
	}
	return out
}
