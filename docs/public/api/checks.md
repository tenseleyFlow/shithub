# Status checks

External CI systems publish status into shithub via the check-runs
API. Branch-protection rules can require named contexts to pass
before a PR merges.

## Concepts

- A **check run** is one invocation: `(repo, head_sha, name)`
  with a state and metadata.
- A **check suite** is the bag of check runs for one head SHA;
  shithub computes it server-side.
- Branch protection's "required status checks" matches by **name**
  on the head commit.

## Create a check run

```
POST /api/v1/repos/{owner}/{repo}/check-runs
```

Required scope: `repo` (write).

### Request body

```json
{
  "name":       "ci/lint",
  "head_sha":   "8c4e3f2a1b…",
  "status":     "in_progress",
  "started_at": "2026-05-09T16:00:00Z",
  "details_url": "https://ci.example/runs/12345",
  "external_id": "12345",
  "output": {
    "title":   "Lint",
    "summary": "Running golangci-lint",
    "text":    "..."
  }
}
```

| Field          | Required | Notes                                                      |
|----------------|----------|------------------------------------------------------------|
| `name`         | yes      | The context name. Match this on branch-protection rules.   |
| `head_sha`     | yes      | 40-char SHA-1 of the commit being checked.                 |
| `status`       | no       | `queued`, `in_progress`, `completed`. Default `queued`.    |
| `conclusion`   | iff `status=completed` | `success`, `failure`, `neutral`, `cancelled`, `timed_out`, `action_required`. |
| `started_at`   | no       | RFC 3339.                                                  |
| `completed_at` | iff `status=completed` | RFC 3339.                                       |
| `details_url`  | no       | Where humans go to inspect the run.                        |
| `external_id`  | no       | Your system's identifier; echoed back on read.             |
| `output`       | no       | `{title, summary, text}`. Summary is markdown.             |

### Response

`201 Created` with the full check-run resource:

```json
{
  "id":         9876,
  "name":       "ci/lint",
  "head_sha":   "8c4e3f2a1b…",
  "status":     "in_progress",
  "conclusion": null,
  "started_at": "2026-05-09T16:00:00Z",
  "details_url": "https://ci.example/runs/12345",
  "external_id": "12345",
  "output":     { "title": "Lint", "summary": "Running golangci-lint" }
}
```

## Update a check run

```
PATCH /api/v1/repos/{owner}/{repo}/check-runs/{id}
```

Required scope: `repo` (write).

Same body shape as create; partial. Typical use: flip
`status: "completed"` with a `conclusion`.

### Conclusion semantics

| Conclusion        | Effect on PR merge gate                                |
|-------------------|--------------------------------------------------------|
| `success`         | Counts as passing.                                     |
| `failure`         | Blocks merge.                                          |
| `neutral`         | Doesn't block; doesn't count as passing either.        |
| `cancelled`       | Blocks merge.                                          |
| `timed_out`       | Blocks merge.                                          |
| `action_required` | Blocks merge with a human-action signal.               |

## List check runs for a commit

```
GET /api/v1/repos/{owner}/{repo}/commits/{sha}/check-runs
```

Required scope: `repo:read`.

Returns the most recent check run **per name** on this commit.
Older runs are still in the database (audit) but not in this
listing.

```json
{
  "total_count": 2,
  "check_runs": [
    {"id": 9876, "name": "ci/lint",  "status": "completed", "conclusion": "success", ...},
    {"id": 9877, "name": "ci/test",  "status": "in_progress", ...}
  ]
}
```

## List check suites for a commit

```
GET /api/v1/repos/{owner}/{repo}/commits/{sha}/check-suites
```

Required scope: `repo:read`.

Returns the rolled-up suite view per `head_sha`:

```json
{
  "total_count": 1,
  "check_suites": [
    {
      "head_sha":  "8c4e3f2a1b…",
      "status":    "in_progress",
      "conclusion": null,
      "checks_url": "/api/v1/repos/owner/repo/commits/8c4e3f2a1b…/check-runs"
    }
  ]
}
```

## Idempotency

Check-run creates with the same `(repo, head_sha, name,
external_id)` are coalesced — re-publishing the same run from a
retried CI job is safe.

## Permissions

The PAT must belong to a user with **write access** to the repo.
Bot accounts representing CI runners are the typical pattern;
they're regular shithub accounts with PATs scoped to `repo` and
nothing else.
