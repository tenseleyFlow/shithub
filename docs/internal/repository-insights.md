# Repository Insights

Repository insights are served from cached snapshots, not computed in the web
request path. The cache is stored in `repo_insight_snapshots` and refreshed by
the `repo:insights_recalc` worker kind.

## Data Sources

- Pulse, Contributors, Commits, and Code frequency come from bounded
  `git log --numstat` scans of the repository default branch.
- Network uses the existing fork relationship (`repos.fork_of_repo_id`) and
  per-row visibility checks before rendering visible forks.
- Traffic currently renders an honest empty state. shithub does not yet collect
  per-repository view, clone, referrer, or popular-content events.

The git scan is capped by `insights.DefaultMaxCommits` so a large repository
cannot pin a worker indefinitely. The worker overwrites the same snapshot row
on every run, making retries idempotent.

## Refresh Flow

`push:process` enqueues `repo:insights_recalc` whenever the repository default
branch advances. Insights pages also enqueue a best-effort refresh when:

- no snapshot exists yet, or
- a snapshot exists but no longer matches `repos.default_branch_oid`.

HTTP handlers render the last snapshot while a stale refresh is queued. Empty
repositories produce a real empty snapshot rather than a placeholder graph.

## Entitlement Gate

Public repository insights are visible to readers. Private organization
repository insights are gated by `entitlements.FeatureRepoInsights`, matching
the GitHub Team-style “Repository insights” row. The gate uses the shared repo
feature path in `internal/web/handlers/repo/collaboration_gates.go`.

## Follow-Up Gap

Traffic is still the remaining SP24 parity gap. A future slice should add a
privacy-conscious event model for repository views/clones, aggregation jobs,
and the Traffic page tables before the plan comparison can mark the whole
repository-insights suite as fully shipped.
