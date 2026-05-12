# Issues

Issues live per repo. They share the `issues` table with pull
requests, but the REST surface here returns issues only (PRs land
in their own section). Markdown bodies are stored raw; the cached
HTML render comes from the same `internal/markdown` pipeline the
web UI uses.

## Issue shape

```json
{
  "id": 17,
  "number": 1,
  "title": "first bug",
  "body": "kaboom",
  "state": "open",
  "state_reason": "",
  "locked": false,
  "lock_reason": "",
  "author_id": 42,
  "labels": ["bug"],
  "created_at": "2026-05-12T05:00:00Z",
  "updated_at": "2026-05-12T05:00:00Z"
}
```

`state` is `"open"` or `"closed"`. `state_reason` is one of
`"completed"`, `"not_planned"`, `"duplicate"`, `"reopened"`, or
empty when no reason has been recorded. `closed_at` is present
only on closed issues.

## List issues

```
GET /api/v1/repos/{owner}/{repo}/issues
```

Required scope: `repo:read`. Paginated; `?per_page=` (≤100) and
`?page=` apply, with the standard `Link:` header.

Optional `?state=open|closed|all` filter; `state=all` (or omitted)
returns both.

Pull requests are not included on this endpoint — fetch them via
the pulls surface.

## Get a single issue

```
GET /api/v1/repos/{owner}/{repo}/issues/{number}
```

Required scope: `repo:read`. `404` when the issue doesn't exist,
when the caller can't see the repo, or when `{number}` belongs to
a pull request (use the pulls surface).

## Create an issue

```
POST /api/v1/repos/{owner}/{repo}/issues
```

Required scope: `repo:write`. Policy: `ActionIssueCreate`.

```json
{ "title": "first bug", "body": "kaboom" }
```

| Field   | Type   | Notes                                         |
|---------|--------|-----------------------------------------------|
| `title` | string | Required, 1–256 chars.                        |
| `body`  | string | Optional markdown body, ≤65535 chars.         |

Returns `201` with the issue envelope.

### Errors

| Status | When                                              |
|-------:|---------------------------------------------------|
|    401 | PAT missing/invalid.                              |
|    403 | PAT lacks `repo:write` scope or policy denial.    |
|    422 | Empty title, title too long, body too long.       |

## Update an issue

```
PATCH /api/v1/repos/{owner}/{repo}/issues/{number}
```

Required scope: `repo:write`. Only the fields you send are
modified; everything else stays.

```json
{
  "title":     "first bug — root cause found",
  "body":      "see comment #3",
  "state":     "closed",
  "state_reason": "completed",
  "labels":    ["bug", "needs-triage"],
  "assignees": ["alice"],
  "milestone": 7
}
```

Permission rules:

- **Title / body** — the issue author OR a repo collaborator with
  write access. Other callers `403`.
- **State / state_reason** — any caller with `ActionIssueClose`
  on the repo. `state_reason` must be one of `completed`,
  `not_planned`, `duplicate`, `reopened` (or empty).
- **Labels** — caller needs `ActionIssueLabel`. The payload is a
  *full replace*: `["bug"]` strips every other label;
  `[]` clears them all. Omit the field to leave labels untouched.
  Unknown label names return `422`.
- **Assignees** — caller needs `ActionIssueAssign`. Same
  full-replace semantics, names are usernames; unknown usernames
  return `422`.
- **Milestone** — caller needs `ActionIssueAssign`. Pass the
  milestone `id` (see [Milestones](#milestones) below); `0`
  clears the milestone. The milestone must belong to the same
  repo; cross-repo ids return `422`.

Returns the freshly-loaded issue.

## Lock and unlock

```
PUT    /api/v1/repos/{owner}/{repo}/issues/{number}/lock
DELETE /api/v1/repos/{owner}/{repo}/issues/{number}/lock
```

Required scope: `repo:write`. PUT body is optional:

```json
{ "lock_reason": "off-topic" }
```

Returns `204`. Locking refuses non-collaborator comments; the
issue itself stays visible.

## Comments

### List

```
GET /api/v1/repos/{owner}/{repo}/issues/{number}/comments
```

Required scope: `repo:read`. Returns comments in chronological
order.

### Add

```
POST /api/v1/repos/{owner}/{repo}/issues/{number}/comments
```

Required scope: `repo:write`. Policy: `ActionIssueComment`. Body:

```json
{ "body": "lgtm" }
```

Subject to the per-author comment rate limit (20/hour); `429`
when exceeded.

### Edit own comment

```
PATCH /api/v1/repos/{owner}/{repo}/issues/comments/{cid}
```

Required scope: `repo:write`. The comment author can edit their
own; other callers `403`.

### Delete a comment

```
DELETE /api/v1/repos/{owner}/{repo}/issues/comments/{cid}
```

Required scope: `repo:write`. The comment author can delete their
own; repo collaborators with write access can delete any comment
on the repo (moderation affordance, matches the gh shape).

Returns `204`.

## Milestones

```
GET    /api/v1/repos/{owner}/{repo}/milestones[?state=open|closed|all]
POST   /api/v1/repos/{owner}/{repo}/milestones
GET    /api/v1/repos/{owner}/{repo}/milestones/{id}
PATCH  /api/v1/repos/{owner}/{repo}/milestones/{id}
DELETE /api/v1/repos/{owner}/{repo}/milestones/{id}
```

Required scope: `repo:read` on GETs, `repo:write` on mutations.
Mutations gate on `ActionIssueLabel` (write collaborator role
on the repo).

Create payload:

```json
{
  "title":       "v1.0",
  "description": "first stable release",
  "due_on":      "2026-06-01T00:00:00Z",
  "state":       "open"
}
```

`due_on` is RFC3339; omit or pass `null` to leave it unset. The
response shape mirrors GitHub's milestone with the repo-local
`id`, the `state` (`open` / `closed`), and live `open_issues` /
`closed_issues` counters.

Identifier: the path takes the milestone primary key `id`
(returned by list / create), not a per-repo number — shithub's
schema doesn't carry a number column. The CLI gets the id back
from `POST` or list responses.

## Assignees

```
GET /api/v1/repos/{owner}/{repo}/assignees
```

Required scope: `repo:read`. Returns the users eligible to be
assigned: the repo owner (when user-owned) plus every direct
repo collaborator. Org-level expansion of org-owned repos is a
follow-up.

## Not yet shipped

`POST /api/v1/repos/{o}/{r}/issues/{n}/transfer` and issue
pinning are queued for a follow-up batch.
