# Pull requests

Pull requests live per repo. They share the `issues` row for
title/body/state/timeline; this surface exposes the PR-specific
shape (refs, oids, mergeability, merge state). Reviews and review
comments live in a sibling batch that ships later.

## Pull request shape

```json
{
  "id": 19,
  "number": 1,
  "title": "wire up foo",
  "body": "first cut",
  "state": "open",
  "draft": false,
  "base_ref": "trunk",
  "head_ref": "feature",
  "base_oid": "abc…",
  "head_oid": "def…",
  "mergeable": true,
  "mergeable_state": "clean",
  "merged": false,
  "author_id": 42,
  "created_at": "2026-05-12T05:00:00Z",
  "updated_at": "2026-05-12T05:00:00Z"
}
```

`mergeable` is omitted while the worker hasn't computed it yet
(treat absence as "unknown"). `mergeable_state` mirrors GitHub's
vocabulary (`clean`, `dirty`, `blocked`, `unknown`, `unstable`,
`has_hooks`, etc. — depending on what the worker recorded).

When `merged` is true, `merge_commit_sha`, `merge_method`, and
`merged_at` are populated.

## List pull requests

```
GET /api/v1/repos/{owner}/{repo}/pulls
```

Required scope: `repo:read`. Paginated; `?per_page=` (≤100) and
`?page=` apply, with the standard `Link:` header.

Optional `?state=open|closed|all` filter (defaults to all);
optional `?draft=true|false` filter.

## Get a single pull request

```
GET /api/v1/repos/{owner}/{repo}/pulls/{number}
```

Required scope: `repo:read`. `404` when the number refers to an
issue (use the issues surface) or when the caller can't see the
repo.

## Create a pull request

```
POST /api/v1/repos/{owner}/{repo}/pulls
```

Required scope: `repo:write`. Policy: `ActionPullCreate`.

```json
{
  "title": "wire up foo",
  "body": "first cut",
  "base": "trunk",
  "head": "feature",
  "draft": false
}
```

| Field   | Type   | Notes                                              |
|---------|--------|----------------------------------------------------|
| `title` | string | Required, 1–256 chars.                             |
| `body`  | string | Optional markdown body, ≤65535 chars.              |
| `base`  | string | Required, branch name on the target repo.          |
| `head`  | string | Required, must differ from `base`.                 |
| `draft` | bool   | Open as draft; flip to ready via PATCH.            |

Returns `201` with the PR envelope. `422` when `head` or `base`
don't resolve, when they're equal, or when no commits are ahead
of `base`.

## Update a pull request

```
PATCH /api/v1/repos/{owner}/{repo}/pulls/{number}
```

Required scope: `repo:write`. Send only the fields you want to
change.

```json
{
  "title": "renamed",
  "body": "updated body",
  "state": "closed",
  "draft": false
}
```

Permission rules:

- **Title / body** — PR author OR a repo collaborator with write
  access. Other callers `403`.
- **State** — `ActionPullClose` on the repo.
- **Draft** — flipping `true` → `false` (mark ready) is
  author-only. `false` → `true` is not supported (`422`).

## List commits in a pull request

```
GET /api/v1/repos/{owner}/{repo}/pulls/{number}/commits
```

Required scope: `repo:read`. Returns the commit set ahead of base
in order, populated by the synchronize worker after open.

## List files changed by a pull request

```
GET /api/v1/repos/{owner}/{repo}/pulls/{number}/files
```

Required scope: `repo:read`. Each entry carries `path`, `status`
(`added`/`modified`/`removed`/`renamed`), `additions`,
`deletions`, and `changes`. `old_path` is set on renames.

## Merge a pull request

```
PUT /api/v1/repos/{owner}/{repo}/pulls/{number}/merge
```

Required scope: `repo:write`. Policy: `ActionPullMerge`.

```json
{
  "commit_title": "Optional override",
  "commit_message": "Optional body",
  "merge_method": "merge",
  "sha": "<head_oid>"
}
```

| Field            | Type   | Notes                                              |
|------------------|--------|----------------------------------------------------|
| `commit_title`   | string | Subject override; falls back to `"<title> (#<number>)"`. |
| `commit_message` | string | Body for the merge commit. Ignored on rebase.      |
| `merge_method`   | string | `"merge"`, `"squash"`, `"rebase"` — must be enabled on the repo. Defaults to the repo's default merge method. |
| `sha`            | string | Optional `head_oid` guard; `409` on mismatch.      |

Returns `200` with the freshly-loaded PR (state now `closed`,
`merged` true). `409` when the PR is already merged/closed or
when `mergeable_state` isn't `clean`. `422` when the requested
merge method is disabled on the repo or when no commits are
ahead of base. `503` if another merge is in flight.

## Update a pull request branch

```
PUT /api/v1/repos/{owner}/{repo}/pulls/{number}/update-branch
```

Required scope: `repo:write` for collaborators, or `repo:read` for the pull
request author. Policy: read access plus author ownership, or `ActionRepoWrite`
on the base repository.

Optional JSON body:

```json
{
  "expected_head_sha": "<current-head-oid>"
}
```

The request updates the pull request head branch with the base branch. It uses
merge strategy by default; set `X-Shithub-Strategy: rebase` to request a rebase.
Returns `200` with a short message and pull request URL. `409` indicates the
head moved, the base moved, or the branch is already up to date. `422` indicates
a non-updateable pull request shape.

## Reviews

PR reviews bundle a verdict (`approve`, `request_changes`, or
`comment`) with optional body and any draft inline comments the
caller had pending on this PR.

### List reviews

```
GET /api/v1/repos/{owner}/{repo}/pulls/{number}/reviews
```

Required scope: `repo:read`. Returns submitted reviews in
creation order.

### Submit a review

```
POST /api/v1/repos/{owner}/{repo}/pulls/{number}/reviews
```

Required scope: `repo:write`. Policy: `ActionPullReview` —
reviewers must be repo collaborators with at least write access.

```json
{ "event": "APPROVE", "body": "looks good to me" }
```

`event` is one of `APPROVE`, `REQUEST_CHANGES`, `COMMENT`
(lowercase accepted too). Submitting `APPROVE` as the PR author
returns `403`. Submitting attaches every pending inline comment
the caller authored on this PR.

## Inline review comments

### List

```
GET /api/v1/repos/{owner}/{repo}/pulls/{number}/comments
```

Required scope: `repo:read`. Returns one entry per inline
comment with `file_path`, `side`, `original_commit_sha`,
`original_line`, `original_position`, optional `current_position`
(absent → outdated), `pending`, `resolved`, and threading via
`in_reply_to_id`.

### Add

```
POST /api/v1/repos/{owner}/{repo}/pulls/{number}/comments
```

Required scope: `repo:write`. Policy: `ActionPullReview`.

```json
{
  "body": "nit: unused import",
  "file_path": "foo.txt",
  "side": "right",
  "original_commit_sha": "abc123",
  "original_line": 12,
  "original_position": 8,
  "current_position": 8,
  "pending": true
}
```

`pending=true` files a draft that's attached to the next review
the caller submits. `in_reply_to_id` threads under an existing
comment and inherits its diff anchor.

## Requested reviewers

### List active

```
GET /api/v1/repos/{owner}/{repo}/pulls/{number}/requested_reviewers
```

Required scope: `repo:read`. Returns active and historical review
requests with `dismissed_at` / `satisfied_by_review_id` set when
applicable.

### Request a reviewer

```
POST /api/v1/repos/{owner}/{repo}/pulls/{number}/requested_reviewers
```

Required scope: `repo:write`. Policy: `ActionPullReview`. Body:

```json
{ "user_id": 42 }
```

or equivalently `{ "username": "bob" }`. `409` when the user is
already an active pending reviewer; `422` on unknown user.

### Dismiss a request

```
DELETE /api/v1/repos/{owner}/{repo}/pulls/{number}/requested_reviewers
```

Required scope: `repo:write`. Same body. Returns `204`; `404`
when there is no active request for that user.

## Not yet shipped

- `PUT /pulls/{n}/auto-merge`
