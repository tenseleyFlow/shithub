# Actions workflow API

Actions workflow lifecycle endpoints are PAT-authenticated and require
`repo:write`. The token's user must also have write permission on the
repository that owns the target run or job.

## Runs Atom feed

```text
GET /{owner}/{repo}/actions.atom
```

Returns the last 50 workflow runs as `application/atom+xml`. Visibility
matches the Actions tab: public repositories are public; private
repositories require repository read access.

Each entry links to the run page and summarizes workflow name, event,
branch, commit, status, and conclusion.

## Cancel job

```text
POST /api/v1/jobs/{id}/cancel
```

Requests cancellation for a workflow job. Queued jobs become terminal
immediately. Running jobs set `cancel_requested=true`; the runner observes
that flag through its cancel-check endpoint, stops the active container, and
reports the terminal status.

Response: `202 Accepted`.

```json
{
  "job_id": 10,
  "run_id": 4,
  "repo_id": 2,
  "changed_jobs": 1,
  "run_completed": false,
  "run_conclusion": ""
}
```

## Re-run workflow run

```text
POST /api/v1/runs/{id}/rerun
```

Creates a new workflow run from the original run's commit and workflow file.
Only terminal runs are rerunnable. The new run records `parent_run_id` so the
history remains linked.

Response: `201 Created`.

```json
{
  "run_id": 12,
  "run_index": 8,
  "parent_run_id": 4,
  "repo_id": 2,
  "workflow_file": ".shithub/workflows/ci.yml",
  "head_sha": "0123456789abcdef0123456789abcdef01234567"
}
```
