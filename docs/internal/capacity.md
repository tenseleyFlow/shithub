# Capacity envelope

Records the load-test results from the S39 hardening sprint and
the rule-of-thumb numbers we use for capacity planning. The
public-facing version is `docs/public/self-host/capacity.md`,
which is summary-only; this file carries the run-by-run detail.

> **Status.** Until the staging environment runs S39's load
> scenarios end-to-end, the numbers below are placeholders
> sourced from S36's bench (which exercises single-user p50/p95
> on the read-heavy paths). Re-populate after the first staging
> load run; track each run as a dated row in the per-scenario
> tables.

## Test environment

- Staging compute matches the production reference deployment
  (`docs/public/self-host/prerequisites.md`):
  - 2× web (2 vCPU / 4 GB)
  - 1× worker (2 vCPU / 4 GB)
  - 1× postgres (2 vCPU / 8 GB / 100 GB SSD)
  - 1× backup, 1× monitoring (smaller)
- Caddy at the edge with TLS terminated.
- WireGuard mesh between hosts.
- Staging seeded with synthetic data:
  - 5,000 users, 50,000 repos, ~500,000 issues, ~1M comments.
  - Largest repo: ~50 MB packed; 95th percentile under 5 MB.

## Per-scenario results

### Mixed-read (anonymous browsing, 100 RPS for 10 min)

| Metric         | S36 single-user | S39 baseline (target ≤2x) | Last actual |
|----------------|-----------------|---------------------------|-------------|
| p50            | 35 ms           | 70 ms                     | TBD         |
| p95            | 80 ms           | 160 ms                    | TBD         |
| p99            | 200 ms          | 400 ms                    | TBD         |
| Error rate     | n/a             | < 1% (ex. 429)            | TBD         |
| Worker queue   | n/a             | bounded                   | TBD         |

### Authenticated mix (50 RPS for 10 min)

| Metric         | S36 single-user | S39 baseline (target ≤2x) | Last actual |
|----------------|-----------------|---------------------------|-------------|
| p50            | 50 ms           | 100 ms                    | TBD         |
| p95            | 150 ms          | 300 ms                    | TBD         |
| p99            | 350 ms          | 700 ms                    | TBD         |

### Issue-comment storm (100 c/s for 5 min)

| Metric                       | Target           | Last actual |
|------------------------------|------------------|-------------|
| Comment POST p95             | < 500 ms         | TBD         |
| Worker queue depth at end    | < 1k             | TBD         |
| Notification fan-out lag     | < 60s            | TBD         |
| DB pool exhaustion errors    | 0                | TBD         |

### Search load (30 RPS for 10 min)

| Metric         | S36 single-user | S39 baseline (target ≤2x) | Last actual |
|----------------|-----------------|---------------------------|-------------|
| p50            | 350 ms          | 700 ms                    | TBD         |
| p95            | 800 ms          | 1600 ms                   | TBD         |

## Degradation thresholds (where to scale)

From the load tests we infer the **first ceiling** each component
hits. These are operator triggers — monitoring rules in
`deploy/monitoring/prometheus/rules.yml` alert below them.

| Trigger                                            | Action                              |
|----------------------------------------------------|-------------------------------------|
| p95 > 1.5 s sustained 10 min                       | Add a second web host.              |
| DB calls/sec > 5k sustained                        | Hunt for an N+1 first; then DB scale. |
| Job queue depth > 5k for 15 min                    | Add a second worker.                |
| `pg_stat_archiver.failed_count > 0`                | See archive-failing runbook.        |
| Web-host disk > 70%                                | Audit largest repos; clean archived. |
| pgxpool exhaustion errors                          | Raise `db.max_conns`; investigate connection leaks. |

## Hosted quota envelope

PAYMENTS SP08 gives hosted organizations hard numbers that can be
adjusted after production cost data exists:

| Plan | Storage quota | Actions minutes/month |
|------|---------------|-----------------------|
| Free | 500 MiB       | 2,000                 |
| Team | 2 GiB         | 3,000                 |

Storage is a combined organization budget across bare repositories and
tracked object storage. The first implementation can recalculate from
`repos.disk_used_bytes`, finalized Actions step log byte counts, and
Actions artifact byte counts. Avatar/attachment/package accounting
must be added as those surfaces gain durable object-size metadata.

Actions minutes are counted from workflow job runtime, rounded up to
the next whole minute, in the current calendar-month period. Quota
counters are repairable projections; hard write gates should recalc
from source of truth before denying work that is close to a limit.

## Notes from the S39 run

To be filled in after the first end-to-end load run on staging:

- **Worker headroom** — at what comment-storm rate does the
  notification fan-out lag exceed 60s?
- **Auth-mix fairness** — does API-only traffic at 50 RPS
  starve UI-rendered traffic, or do they coexist cleanly under
  pgxpool?
- **Search hot-paths** — the search-load scenario's query
  distribution is synthetic; record which queries dominated the
  p95 tail and whether the indexes covered them.
- **Caddy throughput** — at 100 RPS, is the edge a bottleneck or
  is the CPU mostly idle?

## Rebaseline cadence

- After every major release that touches a hot path.
- Quarterly.
- After any infrastructure change to the staging shape.

Each rebaseline replaces the "Last actual" column. Significant
regressions get a row in the post-mortem section below.

## Regression history

(Empty until the first run completes. Format:
`YYYY-MM-DD — <scenario> — <metric> regressed from X to Y; root
cause / fix.`)
