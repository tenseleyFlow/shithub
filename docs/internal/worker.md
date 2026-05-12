# Worker

The worker pool drains the Postgres-backed job queue introduced in S14.
One binary serves the web API, the SSH dispatcher, the hooks, and the
worker — `shithubd worker` boots a long-running process whose only job
is to dequeue and run.

## Architecture

```
post-receive  ──INSERT push_event + INSERT job + NOTIFY──▶  Postgres jobs
                                                                  │
                                                                  ▼
                                                         shithubd worker
                                                  ┌──────────────────────┐
                                                  │ N goroutines          │
                                                  │ ClaimJob (FOR UPDATE  │
                                                  │   SKIP LOCKED)        │
                                                  │ → handler             │
                                                  │ → MarkCompleted /     │
                                                  │   Reschedule /        │
                                                  │   MarkFailed          │
                                                  └──────────────────────┘
```

The pool also runs a dedicated LISTEN goroutine that holds one Postgres
connection on `LISTEN shithub_jobs`. A NOTIFY from any process (typically
the post-receive hook) wakes idle workers in <1s without polling. The
backstop poll (every 5s by default) covers dropped notifications.

## Job kinds shipped in S14

| Kind                         | Trigger                              | Idempotent on                    |
| ---------------------------- | ------------------------------------ | -------------------------------- |
| `push:process`               | post-receive hook per ref            | `push_events.processed_at`       |
| `repo:size_recalc`           | enqueued by `push:process`           | overwrite-last-wins              |
| `org:github_import_discover` | org import request                   | `org_github_imports.status`      |
| `org:github_import_repo`     | import discovery per GitHub repo     | `org_github_import_repos.status` |
| `jobs:purge_completed`       | cron / manual ad-hoc                 | always safe to re-run            |
| `workflow:cleanup`           | cron / manual ad-hoc                 | retention cutoff + idempotent deletes |
| `trending:compute`           | recurring self-scheduled S42 job     | append-only snapshots            |

Adding a new kind: write the handler in `internal/worker/jobs/<kind>.go`,
add the `Kind` constant to `internal/worker/types.go`, register it in
`cmd/shithubd/worker.go`. The dispatch loop is generic — no other code
needs to change.

## Failure handling

* **Transient errors** (handler returns a non-nil non-poison error):
  reschedule with `Backoff(attempts) ± 20% jitter`, where `Backoff` is
  `30s * 2^(attempts-1)` capped at 1h.
* **Poison errors** (handler returns an error wrapping `worker.ErrPoison`):
  jump to `MarkJobFailed` immediately. Use this for malformed payloads or
  references to vanished rows.
* **Panics**: caught by `safeRun` and treated as a transient error so a
  buggy handler can't take down a worker goroutine.
* **Stuck locks**: `ClaimJob` ignores rows whose `locked_at` is older
  than 5 minutes — a worker that died mid-job releases its rows on the
  next claim cycle.
* **Max attempts**: `jobs.max_attempts` (default 5). Past the limit the
  row is moved to `failed_at`. Failed rows surface in the S34 admin
  panel for poison-job triage.

## Concurrency contract

* `FOR UPDATE SKIP LOCKED` on the inner SELECT means N concurrent workers
  on the same kind can claim N distinct rows in one round. Each row is
  processed exactly once across the cluster.
* Workers hold a row's lock only while their handler runs. The lock is
  released atomically by `MarkJobCompleted` / `RescheduleJob` /
  `MarkJobFailed`.
* Job handlers MUST be idempotent. A worker process killed mid-handler
  leaves the row locked until the 5-minute stale-lock window passes,
  at which point another worker may pick it up and re-run. Guard the
  side-effects (e.g. `processed_at IS NULL` checks).

## Metrics

All exported through the standard `/metrics` registry:

* `shithub_worker_jobs_processed_total{kind,outcome}` — outcome ∈
  `ok | retry | failed | poison`.
* `shithub_worker_job_duration_seconds{kind}` — handler latency
  histogram.
* `shithub_worker_in_flight{kind}` — gauge of currently-running handlers.

## Operational notes

* Default pool size is 4. Override via `--workers <n>` on the CLI or
  `SHITHUB_WORKERS=<n>` in the environment.
* Graceful shutdown: SIGINT/SIGTERM cancels the root context. Workers
  stop pulling new jobs and let in-flight handlers finish (bounded by
  `JobTimeout`, default 5 minutes). The LISTEN goroutine drops its
  conn cleanly.
* The pool size on the Postgres connection is sized to `Workers + 2`
  (one for LISTEN, one slack for enqueues during shutdown).
* `LISTEN/NOTIFY` semantics: when the hook calls `pg_notify` inside a
  transaction, the notification is *only* delivered after commit. So
  failed inserts never wake workers for nothing.
