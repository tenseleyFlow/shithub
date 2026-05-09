# Caching

The S36 perf-pass standardises on an in-process LRU
(`internal/cache/lru`) with optional TTL and a single-flight
wrapper for hot-key dogpile prevention. This doc tracks every
cross-request cache and its invalidation contract.

The invariant is: **every cached value has a documented
invalidation trigger**. If you can't name the trigger, the cache
is a bug factory.

## Active caches

| Cache | Key | Value | Invalidator | Bound |
|---|---|---|---|---|
| `repos/git.AheadBehindCached` | (repo_id, base_oid, head_oid) | (ahead, behind) | OID change ⇒ different key; LRU eviction | 4096 entries |

Concrete uses:
- `branchesList` (S20 deferral H4) — replaces N `git rev-list`
  invocations per page load with one cached lookup per branch.
  Single-flight collapses concurrent misses on hot branches.

## Planned caches (next iterations)

These are listed in the S36 spec; they land as the surfaces they
back grow large enough to bench-justify the cache.

| Cache | Key | Value | Invalidator |
|---|---|---|---|
| Tree at root | (repo_id, ref_oid) | rendered ls-tree result | push:process bumps default-OID |
| Ref list | (repo_id) | branches + tags | push:process |
| File list (finder) | (repo_id, ref_oid) | flat path slice | push:process |
| Default-branch OID | (repo_id) | OID string | push:process + default-branch swap |
| Markdown render | (markdown_pipeline_version, body_hash) | rendered HTML | bump pipeline version on goldmark/policy change |
| Effective-team-set | (actor, org) | team-id slice | team-membership change |

## Single-flight: when to wrap

Wrap with `lru.Group` whenever:

1. The upstream is non-trivial (subprocess, FS walk, multi-row DB read), AND
2. The key is hot (one popular repo, one busy user), AND
3. Concurrent misses are realistic (HTTP burst, worker fanout).

Without single-flight, a cache miss under load triggers a stampede
that defeats the cache's purpose. The `lru.Group` wrapper collapses
N concurrent misses into one upstream call.

## Errors are NOT cached

`lru.Group.Do` deliberately returns errors without caching them. A
transient upstream failure (DB blip, git fork EAGAIN) shouldn't
poison subsequent reads. Negative caching (caching the absence of
a key) is a separate concern; callers add their own sentinel value
when they want it.

## TTL: when to use one

Default to no-TTL with explicit invalidation. Use TTL only when:

- The data is fully public + anonymous-cacheable (rendered HTML for
  a public repo's README).
- Staleness is measured-low-impact (≤ 60s for hot reads).
- An explicit invalidator is wired in addition (TTL is the safety
  net, not the primary correctness mechanism).

Avoid TTL on personalized content. The "you see your friend's old
comment for 30s" UX surprise is not worth the cache hit-rate.

## Reading hit-rates

Every cache exposes `Stats() lru.Stats{Hits, Misses, Evictions}`.
The `/metrics` surface (S37 deploy) will scrape these. CI baseline
asserts hit-rate above a per-cache target on the bench run.

## Invalidation patterns

The push:process worker is the canonical invalidation source for
git-shaped caches. After updating refs:

```go
git.InvalidateAheadBehind(git.AheadBehindKey{...})
// + future: tree, refs, default-branch caches
```

The (repo_id, ...) key shape lets us scope invalidation to one
repo's slice without scanning the whole cache.
