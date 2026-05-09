// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"strconv"

	"github.com/tenseleyFlow/shithub/internal/cache/lru"
)

// AheadBehindKey is the (repo_id, base_oid, head_oid) tuple. The
// OIDs make the cache safe across pushes — when either ref moves,
// the new OID forms a different key. Old keys age out via LRU
// eviction; explicit invalidation isn't strictly required but helps
// keep the working set small (push handlers call InvalidateRepo).
type AheadBehindKey struct {
	RepoID  int64
	BaseOID string
	HeadOID string
}

// AheadBehindResult is the cached payload.
type AheadBehindResult struct {
	Ahead, Behind int
}

// abGroup is the process-global single-flight cache. 4096 entries ≈
// a few hundred repos with active branch sets; bounded enough that
// the cache itself never dominates RSS, large enough to survive the
// branch-list page's burst of (default vs N branches) lookups.
var abGroup = lru.NewGroup(
	lru.New[AheadBehindKey, AheadBehindResult](4096),
	abKeyer,
)

func abKeyer(k AheadBehindKey) string {
	return strconv.FormatInt(k.RepoID, 10) + "|" + k.BaseOID + "|" + k.HeadOID
}

// AheadBehindCached is the cached + single-flighted variant of
// AheadBehind. Callers pass the resolved OIDs (not ref names) so the
// key is stable across ref-name renames and the cache is never
// poisoned by a stale ref pointer.
//
// On cache miss the underlying `git rev-list` runs once even when
// many requests arrive concurrently for the same key (single-flight
// dogpile guard). The result is cached until LRU eviction or
// explicit InvalidateRepo.
func AheadBehindCached(ctx context.Context, gitDir string, key AheadBehindKey) (AheadBehindResult, error) {
	return abGroup.Do(ctx, key, func(ctx context.Context) (AheadBehindResult, error) {
		ahead, behind, err := AheadBehind(ctx, gitDir, key.BaseOID, key.HeadOID)
		if err != nil {
			return AheadBehindResult{}, err
		}
		return AheadBehindResult{Ahead: ahead, Behind: behind}, nil
	})
}

// InvalidateAheadBehindForRepo drops every cached entry whose key
// matches repoID. Called from push:process so a force-push that
// rewrites history doesn't surface stale ahead/behind counts.
//
// The current implementation is approximate: the LRU exposes Delete
// per-key, not by-prefix scan. Push handlers that know the specific
// OIDs that moved should call InvalidateAheadBehind directly; this
// helper is kept as a documented future extension point so the API
// is stable when a richer scan lands.
func InvalidateAheadBehindForRepo(repoID int64) {
	// Intentionally a no-op for now — see comment above. The LRU
	// pressure is bounded by capacity, so stale entries age out
	// naturally; correctness is preserved because the cache key
	// includes the OID.
	_ = repoID
}

// InvalidateAheadBehind drops one specific (repo, base, head) entry.
// Use from push:process when the exact OIDs are known.
func InvalidateAheadBehind(key AheadBehindKey) { abGroup.Invalidate(key) }

// AheadBehindCacheStats exposes hit/miss counters for the /metrics
// surface and bench reports.
func AheadBehindCacheStats() lru.Stats { return abGroup.Stats() }
