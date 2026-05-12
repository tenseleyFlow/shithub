# Actions lifecycle

State-changing endpoints for workflows, runs, and artifacts. The
cancel + rerun routes from the S41g actions-lifecycle surface
are documented separately in
[Actions workflow API](./actions.md); this page covers the
S50 §13 REST additions.

Scopes:

- `repo:read` on the artifact reads + job-log download
- `repo:write` on enable/disable, run delete, artifact delete

## Endpoints

```
PUT    /api/v1/repos/{owner}/{repo}/actions/workflows/{workflow}/enable
PUT    /api/v1/repos/{owner}/{repo}/actions/workflows/{workflow}/disable
DELETE /api/v1/repos/{owner}/{repo}/actions/runs/{run_id}
GET    /api/v1/repos/{owner}/{repo}/actions/runs/{run_id}/artifacts
GET    /api/v1/repos/{owner}/{repo}/actions/artifacts/{artifact_id}
GET    /api/v1/repos/{owner}/{repo}/actions/artifacts/{artifact_id}/zip
DELETE /api/v1/repos/{owner}/{repo}/actions/artifacts/{artifact_id}
GET    /api/v1/repos/{owner}/{repo}/actions/jobs/{job_id}/logs
```

## Enable / disable a workflow

```
PUT /api/v1/repos/alice/demo/actions/workflows/ci.yml/disable
```

Idempotent. Returns `204 No Content`. While a workflow is
disabled, matching events (push, PR, dispatch, schedule) do not
enqueue runs — the trigger pipeline consults the
`workflow_disabled` row and short-circuits with `Skipped: true`.
Re-enabling with PUT `/enable` removes the row and resumes
triggering on the next event.

The workflows list endpoint reports disabled workflows with
`"state": "disabled"`.

## Delete a workflow run

```
DELETE /api/v1/repos/alice/demo/actions/runs/42
```

Returns `204 No Content`. Cascades to all dependent rows
(`workflow_jobs`, `workflow_steps`, `workflow_step_log_chunks`,
`workflow_artifacts`). Artifact blobs in object storage are
best-effort cleaned up asynchronously after the row is gone; the
cleanup sweeper picks up any orphans left behind.

Returns `404` if the run id is unknown OR if the run belongs to a
different repo (existence-leak-safe).

## Artifacts

```
GET /api/v1/repos/alice/demo/actions/runs/42/artifacts
```

```json
[
  {
    "id":          17,
    "name":        "logs",
    "size_bytes":  4096,
    "archive_url": "https://shithub.example/api/v1/repos/alice/demo/actions/artifacts/17/zip",
    "expires_at":  "2026-06-12T18:00:00Z",
    "created_at":  "2026-05-12T18:00:00Z"
  }
]
```

`archive_url` points at the download endpoint:

```
GET /api/v1/repos/alice/demo/actions/artifacts/17/zip
```

Streams the artifact bytes directly from the object store with
`Content-Type: application/zip` and a sensible
`Content-Disposition`. No redirect — the bytes come back inline.

`DELETE /api/v1/repos/alice/demo/actions/artifacts/17` removes the
DB row + queues the blob for async cleanup. `204` on success;
`404` when the artifact id is unknown or belongs to a different
repo.

## Job logs

```
GET /api/v1/repos/alice/demo/actions/jobs/119/logs
```

Returns `text/plain` with the assembled log transcript across all
steps in the job. Each step is wrapped in `##[group]` /
`##[endgroup]` markers so the output remains readable when piped
or `less`'d. Order matches the step execution order.

## Errors

| Status | Cause                                                       |
|------:|--------------------------------------------------------------|
| 400   | Workflow file path malformed (enable/disable).               |
| 403   | PAT lacks the required scope.                                 |
| 404   | Repo / run / artifact / job unknown, or owned by a different repo. |
| 503   | Object store unavailable (artifact download path).           |
