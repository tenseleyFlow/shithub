# Performance benchmarking

The S36 perf-pass ships a small in-repo benchmark harness so perf
regressions are visible in PR review, not discovered in production.

## Targets (warm cache, MVP hardware)

The S36 spec defines:

- Tree of root on 100k-file repo: < 200ms p95
- Blame on 10k-line file: < 500ms p95
- Commits list page on 1M-commit repo: < 250ms p95
- Issue list (state filter) on 100k issues: < 200ms p95
- Notifications inbox first page: < 150ms p95

These targets assume the big-fixture generators land. Until they
do, `make bench-small` runs against the dev seed and serves as a
floor regression detector for the harness itself + handler latency
on the small dataset.

## Running

```sh
# Defaults: target=http://localhost:8080, iters=20.
make bench-small

# Pin a different target / iteration count.
BENCH_TARGET=http://staging.shithub.sh BENCH_ITERS=100 make bench-small
```

Output is one JSON line per scenario:

```json
{"scenario":"home","iters":20,"ok_count":20,"p50_us":432,"p95_us":5692,"p99_us":5692,"max_us":5692,"mean_us":693.15}
```

`ok_count == 0` for a scenario means every probe missed the
expected status (typically: the dev seed doesn't have the repo the
scenario targets). The harness keeps running and reports zeros so
the suite doesn't bail mid-run.

## Adding scenarios

Append to `bench/run.go::main`'s `scenarios` slice:

```go
{"my-scenario", "GET", "/some/path", 200},
```

Scenarios are intentionally URL-shape probes, not deep state
manipulators. If a scenario needs a logged-in user or a specific
repo state, prefer staging via `make seed` over making the
harness write its own state.

## N+1 query auditing

Wire a route's integration test to assert max-queries:

```go
import "github.com/tenseleyFlow/shithub/internal/web/middleware"

r.Use(middleware.CountQueries())
// ... drive a request through r ...
if got := middleware.QueriesFor(req); got > 8 {
    t.Fatalf("issuesList ran %d queries; threshold 8", got)
}
```

The pgx tracer (`internal/infra/db.QueryCounter`) increments a
per-context counter on every Query/QueryRow/Exec; the middleware
opts the request context in. Production paths pay one
context.WithValue per request — cheap, but the assertion only
fires in tests.

## Big-fixture plan (deferred)

`bench/fixtures/README.md` documents the planned generators for
the 1M-commit / 100k-file / 100k-issue / 5k-member-org fixtures.
They aren't generated yet — the seed cost is non-trivial and the
small dev seed is sufficient as a floor regression detector while
the perf surface is still settling. When the generators land,
`make bench-full` (currently a stub) hooks them up.

## Profile dumps

`bench/profiles/` is the canonical home for `pprof` captures
referenced by the perf docs. The S36 spec asks for profile dumps
for the slowest scenarios; until the big fixtures land, the
captures are dev-machine pprof of `make bench-small` and aren't
checked in by default.
