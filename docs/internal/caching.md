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

## Template renderer: one per process

`*render.Renderer` is not a cache but it is the largest single
static allocation in `shithubd web`, so the invariant lives here.

**There is exactly one `render.Renderer` per process.** It is built
in `internal/web/server.go` and threaded into every handler builder
(`buildAuthHandlers`, `buildRepoHandlers`, …) and into
`handlers.Deps.Renderer`. Handler builders take a
`*render.Renderer`, never an `fs.FS` to build one from.
`internal/web/renderer_heap_test.go` enforces both halves: an AST
scan that fails on a second `render.New` anywhere in package web,
and a live-heap ceiling of 150 MB.

Why it matters: `render.New` parses every page template into its
own `*template.Template` set. html/template cannot share parse
trees across sets — the contextual auto-escaper rewrites trees in
place, per set — so each page holds a private copy of every partial
it reaches. That is ~40 MB of live heap for the current 153 pages.
Before 2026-09 the wiring built one renderer per handler set, eight
in all, for ~664 MB of *static* heap on a 3.9 GB box; that is what
OOM-killed `shithubd`. See
`docs/internal/retro/2026-09-02-availability-sitrep.md`.

Second lever, already applied: `render.New` parses into each page
only the partials that page transitively references, seeded from
`"layout"`. Pruning the unreachable ones cut a renderer from 83 MB
to ~40 MB. `internal/web/render/parity_test.go` renders all 153
production pages through both the pruned and the parse-everything
loader and requires byte-identical output, so the reachability
analysis cannot silently drop a partial.

What is left: `_layout.html` is 32 KB of the ~54 KB every page
still pulls in, and it is a single monolithic `{{ define "layout" }}`.
Splitting rarely-used chrome out of it is the next material win
(~40 MB → ~20 MB), but it is a template refactor, not a loader
change.
