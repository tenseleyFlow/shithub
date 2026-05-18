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
