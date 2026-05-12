# Commits

Read-only commits surface. Mirrors GitHub's
`/repos/{owner}/{repo}/commits` family — backed by `git log`
on the bare repository so the response is always in lock-step
with the on-disk history.

All endpoints require `Authorization: Bearer <pat>` with
`repo:read` and gate on `ActionRepoRead`. The
[common API conventions](overview.md) apply.

## List commits

```
GET /api/v1/repos/{owner}/{repo}/commits[?sha=&path=&author=&since=&until=&page=&per_page=]
```

Query parameters:

- `sha` — branch / tag / commit SHA to start from. Default:
  the repo's `default_branch`.
- `path` — show only commits touching this path (passes
  through to `git log -- <path>`).
- `author` — `git log --author=<...>` filter (substring match
  on author name or email).
- `since` / `until` — RFC3339 timestamps bracketing the window.
- `page` / `per_page` — standard pagination (per_page ≤ 100,
  default 30). Since `git log` doesn't expose a cheap total
  count, the `Link:` header only emits `next` / `prev` rels,
  never `last`.

Response shape (one entry):

```json
{
  "sha":       "5f3a…",
  "short_sha": "5f3aabc",
  "subject":   "fix race in fanout worker",
  "body":      "longer commit body wraps here",
  "author":    {
    "name":  "Alice",
    "email": "alice@example.com",
    "date":  "2026-05-12T00:00:00Z"
  }
}
```

Empty / uninitialised repos return `[]` rather than `404`.

## Get a commit

```
GET /api/v1/repos/{owner}/{repo}/commits/{sha}
```

`{sha}` accepts a full 40-char SHA or any unambiguous prefix
that `git rev-parse` resolves. Unknown SHAs `404`.

```json
{
  "sha":       "5f3a…",
  "short_sha": "5f3aabc",
  "subject":   "fix race in fanout worker",
  "body":      "…",
  "author":    { "name": "Alice", "email": "alice@example.com", "date": "2026-05-12T00:00:00Z" },
  "committer": { "name": "Alice", "email": "alice@example.com", "date": "2026-05-12T00:00:00Z" },
  "parents":   ["9c1b…"],
  "tree_sha":  "abcd…",
  "files": [
    {
      "path":      "internal/webhook/fanout.go",
      "status":    "M",
      "additions": 7,
      "deletions": 3,
      "binary":    false
    }
  ],
  "stats": { "additions": 7, "deletions": 3, "total": 10 }
}
```

`files[].status` is git's letter code (`A` added, `M` modified,
`D` deleted, `R` renamed, `C` copied, `T` type-changed). Renames
and copies surface the original path on `old_path`.
