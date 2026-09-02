# Repository Insights

Repository insights are served from cached snapshots, not computed in the web
request path. The cache is stored in `repo_insight_snapshots` and refreshed by
the `repo:insights_recalc` worker kind.

## Data Sources

- Pulse, Contributors, Commits, and Code frequency come from bounded
  `git log --numstat` scans of the repository default branch.
- Network uses the existing fork relationship (`repos.fork_of_repo_id`) and
  per-row visibility checks before rendering visible forks.
- Traffic comes from aggregate-only repository view and clone counters in
  `repo_traffic_daily`, with popular-content and external-referrer rollups in
  `repo_traffic_paths` and `repo_traffic_referrers`.

The git scan is capped by `insights.DefaultMaxCommits` so a large repository
cannot pin a worker indefinitely. The worker overwrites the same snapshot row
on every run, making retries idempotent.

Traffic collection is intentionally privacy-conscious. Request handlers pass a
short-lived visitor key to `internal/repos/traffic`; the database stores only a
repo/day/metric-scoped SHA-256 digest in `repo_traffic_uniques`. Raw IP
addresses, user agents, authenticated user IDs, and full referrer URLs are not
persisted. Referrers are reduced to an external host and same-site referrers are
dropped.

## Traffic retention

The traffic tables are pruned by the `traffic:purge` worker job, enqueued
nightly from `deploy/systemd/shithubd-cron.service`. Retention windows live in
`internal/repos/traffic/purge.go`:

| Table | Window | Cutoff column | Why |
|---|---|---|---|
| `repo_traffic_uniques` | 30 days | `created_at` | one row per visitor digest per repo/day/metric |
| `repo_traffic_paths` | 30 days | `day` | one row per distinct path per repo/day; crawlers inflate this hardest |
| `repo_traffic_referrers` | 30 days | `day` | one row per external host per repo/day |
| `repo_traffic_daily` | 400 days | `day` | one row per repo/day; the only long-term history, and small enough to keep |

Thirty days is deliberately more than double the fourteen the Traffic UI reads
(`traffic.DefaultWindowDays`), so a purge can never eat a bar the chart would
draw. `repo_traffic_uniques` is filtered on `created_at` rather than `day`
because that is the column it already has an index on, and the request path
stamps both from the same instant.

The job deletes in batches of 5,000 rows, each its own statement, up to 2,000
batches per table per run; the payload (`retention_days`, `daily_retention_days`,
`batch_size`, `max_batches`) overrides any of that for an ad-hoc
`shithubd admin run-job traffic:purge`. Nothing is done in one big transaction:
the tables were left unpruned from 2026-05-18 until the 2026-09-02 availability
sitrep, by which point they held 881 MB of a 988 MB database, and a single
DELETE over that backlog would have locked the write path for minutes. A run
that stops on the batch cap re-enqueues itself so the backlog drains without
waiting for the next cron beat. Re-running is always safe — the cutoff is
recomputed from the clock and rows inside the window are never touched.

## Refresh Flow

`push:process` enqueues `repo:insights_recalc` whenever the repository default
branch advances. Insights pages also enqueue a best-effort refresh when:

- no snapshot exists yet, or
- a snapshot exists but no longer matches `repos.default_branch_oid`.

HTTP handlers render the last snapshot while a stale refresh is queued. Empty
repositories produce a real empty snapshot rather than a placeholder graph.

Existing repositories can be reconciled after deploy with:

```sh
shithubd repo-insights-backfill-all
```

The command enqueues one `repo:insights_recalc` job per active repository and
returns immediately; it does not compute git history inline.

## Entitlement Gate

Public repository insights are visible to readers. Private organization
repository insights are gated by `entitlements.FeatureRepoInsights`, matching
the GitHub Team-style “Repository insights” row. The gate uses the shared repo
feature path in `internal/web/handlers/repo/collaboration_gates.go`.
