# Load tests

k6 scenarios used in the S39 hardening sprint. Each script is a
single load shape; combine them by running concurrently from a
load-generator host (the staging instance must NOT receive load
from the production droplet).

## Scenarios

| Script                              | Shape                                                                 |
|-------------------------------------|-----------------------------------------------------------------------|
| `scenarios/mixed-read.js`           | 100 RPS anonymous browsing of public repos for 10 min                |
| `scenarios/auth-mix.js`             | 50 RPS authenticated API + browsing for 10 min                       |
| `scenarios/issue-comment-storm.js`  | 100 comments/sec across 50 issues for 5 min                          |
| `scenarios/search-load.js`          | 30 RPS to search endpoints for 10 min                                |

Two scenarios from the S39 spec are not yet shipped here:

- **Push storm** — needs a Git client harness, not k6's HTTP runner.
  Track in a follow-up.
- **SSH connection burst** — needs a real SSH client; the SSH
  transport itself isn't shipped (see
  `docs/public/user/ssh.md`). When SSH lands, add an SSH-specific
  scenario.

## Running locally against the dev server

```sh
make dev-db dev-storage dev-migrate dev
# in another terminal:
make load-test BENCH_TARGET=http://127.0.0.1:8080
```

`make load-test` runs the mixed-read scenario by default; tune
with `K6_SCENARIO=auth-mix make load-test` etc.

## Running against staging

```sh
export BASE=https://staging.shithub.example
export TOKEN=shp_<a-pat-on-a-test-account>
export REPO=loadtest/issue-storm  # for the comment-storm scenario
export FIRST_ISSUE=1

k6 run -e BASE -e TOKEN tests/load/k6/scenarios/auth-mix.js
k6 run -e BASE                     tests/load/k6/scenarios/mixed-read.js
k6 run -e BASE -e TOKEN -e REPO -e FIRST_ISSUE tests/load/k6/scenarios/issue-comment-storm.js
k6 run -e BASE                     tests/load/k6/scenarios/search-load.js
```

## Thresholds

`thresholds.json` is shared across scenarios. Default thresholds:

- HTTP failure rate < 1% (excluding rate-limit 429s, which k6
  doesn't classify as failures).
- p95 < 3000 ms.
- p99 < 5000 ms.
- Per-check pass rate > 99%.

These come from the S39 spec: "p95 < 2x of S36's single-user
bench numbers under sustained load." Tighten them once
production has run for a quarter.

## What good looks like

After a clean run, capture the JSON output:

```sh
k6 run --out json=results.json tests/load/k6/scenarios/mixed-read.js
```

Summarise per-scenario p50/p95/p99 + total RPS into the capacity
record (`docs/internal/capacity.md`). The numbers there are
"S39 baseline at MVP launch"; subsequent hardening sprints diff
against them to catch regressions.
