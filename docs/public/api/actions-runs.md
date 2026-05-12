# Actions workflow runs

Read-only REST surface over the `workflow_runs` / `workflow_jobs`
tables. Mirrors GitHub's `/repos/{o}/{r}/actions/runs` endpoints
so the CLI's `gh run list` / `gh run view` / `gh run watch`
flows have a real REST contract.

Scope: `repo:read`. Policy gate: `ActionRepoRead`.

Lifecycle controls (cancel, rerun, approve) live on the
separate Actions lifecycle routes — this surface is **read-only**.

## Endpoints

```
GET /api/v1/repos/{o}/{r}/actions/runs[?…]
GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}
GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}/jobs
```

## List runs

Query parameters (all optional, all combinable):

- `workflow_file` — exact path match (e.g. `.shithub/workflows/ci.yml`).
- `head_ref` — branch / tag name the run was triggered against.
- `actor` — username of the user who triggered the run.
- `event` — `push` / `pull_request` / `workflow_dispatch` / …
- `status` — `queued` / `running` / `completed` / …
- `conclusion` — `success` / `failure` / `cancelled` / …
- `page` / `per_page` — standard pagination (≤100; default 30).

Returns recency-sorted runs with the canonical envelope:

```json
[
  {
    "id":             1042,
    "run_number":     27,
    "workflow_file":  ".shithub/workflows/ci.yml",
    "workflow_name":  "CI",
    "head_sha":       "5f3a…",
    "head_ref":       "trunk",
    "event":          "push",
    "status":         "completed",
    "conclusion":     "success",
    "actor_id":       42,
    "actor_username": "alice",
    "started_at":     "2026-05-12T15:00:00Z",
    "completed_at":   "2026-05-12T15:03:21Z",
    "created_at":     "2026-05-12T14:59:55Z",
    "updated_at":     "2026-05-12T15:03:22Z"
  }
]
```

`Link:` headers carry the standard `first`/`prev`/`next`/`last`
pagination cursors.

## Get a single run

```
GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}
```

Cross-repo probes return 404 — a run id is unique globally, but
the response status doesn't let you enumerate.

## List a run's jobs

```
GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}/jobs
```

```json
[
  {
    "id":              7150,
    "run_id":          1042,
    "job_index":       0,
    "job_key":         "build",
    "job_name":        "Build & test",
    "runs_on":         "ubuntu-latest",
    "status":          "completed",
    "conclusion":      "success",
    "cancel_requested": false,
    "needs_jobs":      [],
    "started_at":      "2026-05-12T15:00:30Z",
    "completed_at":    "2026-05-12T15:03:15Z",
    "created_at":      "2026-05-12T14:59:55Z"
  }
]
```

Jobs come back in `job_index` order so the listing matches the
workflow's declared graph. `needs_jobs` is the literal
dependency array from the workflow file — clients can render the
DAG without re-parsing YAML.

## Errors

- `404` — repo not visible, or run id doesn't belong to this repo.
- `403` — PAT lacks `repo:read`.
