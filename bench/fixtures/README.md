# Bench fixtures

S36 calls for four big-repo fixtures used by the nightly perf run:

- `big_repo_1m_commits/` — repository with 1M commits across a sensible branch graph
- `repo_100k_files/`   — repository with 100k files in a reasonable directory structure
- `issues_100k/`       — repo with 100k issues + 1M comments
- `users_org_5k_members/` — org with 5k members and 200 teams (one level)

These fixtures aren't generated yet — the seed cost is non-trivial
(tens of minutes per fixture) and the current repo shape doesn't
yet warrant the disk-and-CI-time spend. The S36 baseline uses the
small dev seed fixtures via `bench/run.go`.

## Generation plan (deferred)

Each fixture has a generator under `bench/fixtures/<name>/seed.go`
that takes a fixed RNG seed and produces a deterministic on-disk
shape. The CI integrity check re-runs the generator and asserts
the resulting tree-hash matches the committed manifest, so a
generator regression doesn't silently change what the bench measures.

Run order (when generators land):

```
go run ./bench/fixtures/big_repo_1m_commits -out=./bench/fixtures/big_repo_1m_commits
go run ./bench/fixtures/repo_100k_files     -out=./bench/fixtures/repo_100k_files
…
```

The fixtures themselves are gitignored (regenerable). The generators
live in source.

## Dev fixture (today)

The `make seed` flow in the repo root produces the small dev
fixtures used by `make bench-small`. That's enough to catch
regressions in the harness itself plus the small-scale handler
latency floor; the big-fixture targets in S36's "Definition of
done" land with the generators above.

## Actions smoke fixture

`actions/smoke-run-only.yml` is the canonical first end-to-end Actions
workflow fixture. Copy it into `.shithub/workflows/smoke.yml` in a
repository with a registered `ubuntu-latest` runner. It intentionally uses only
`run:` steps because the current executor rejects reserved `uses:` aliases
until checkout and artifact execution are implemented.
