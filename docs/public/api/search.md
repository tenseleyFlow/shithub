# Search

Per-type search over shithub's Postgres FTS corpus. Every result
set is filtered by the caller's visibility — anonymous callers
see only public rows.

## Response shape

Every endpoint returns the canonical gh-compatible envelope:

```json
{
  "total_count": 142,
  "incomplete_results": false,
  "items": [
    { /* type-dependent record */ }
  ]
}
```

`incomplete_results` is always `false` in v1 (the FTS pipeline
runs to completion before responding). The field is preserved on
the wire so future ranker timeouts can flip it.

`total_count` is the unbounded result count.

## Query syntax

The `q=` parameter is parsed by `internal/search/query_parse` —
free-text terms plus the following operators:

- `repo:owner/name` — restrict to one repo
- `is:open` / `is:closed` (or `state:open` / `state:closed`)
- `author:username`
- `"quoted phrase"` — phrase search (one phrase per query)

Unknown prefixes (e.g. `language:Go`) fall through as free text
so future operator additions don't break older queries.

## Search repositories

```
GET /api/v1/search/repositories?q=...&page=N&per_page=M
```

Scope: `repo:read` for authenticated callers; anonymous callers
fall through to the public-only filter.

```json
{
  "total_count": 1,
  "incomplete_results": false,
  "items": [
    {
      "id": 12,
      "name": "demo",
      "full_name": "alice/demo",
      "owner_login": "alice",
      "description": "demo repo",
      "visibility": "public",
      "private": false,
      "star_count": 0,
      "updated_at": "2026-05-12T05:00:00Z",
      "score": 0.42
    }
  ]
}
```

## Search issues

```
GET /api/v1/search/issues?q=...&type=issue|pr
```

Scope: `repo:read`. `type=issue` or `type=pr` narrows to one
kind; omit to return both (issues share their table with PRs).

```json
{
  "id": 17,
  "number": 1,
  "repo_id": 12,
  "repo": "alice/demo",
  "title": "needle in haystack",
  "state": "open",
  "kind": "issue",
  "author_name": "alice",
  "updated_at": "2026-05-12T05:00:00Z",
  "score": 0.81
}
```

## Search code

```
GET /api/v1/search/code?q=...
```

Scope: `repo:read`. Matches against the path-and-content index
populated by the push pipeline. `preview_line` carries a single
line of context for content hits; path-only hits omit it.

```json
{
  "repo_id": 12,
  "repo": "alice/demo",
  "ref": "trunk",
  "path": "internal/foo/foo.go",
  "preview_line": "func Foo() error { return errors.New(\"foo\") }",
  "score": 0.55
}
```

## Pagination

`?per_page=` (≤100, default 30) and `?page=` are supported, with
the standard `Link:` header (see [overview](overview.md)). The
`total_count` field is independent of the page slice — it counts
all matching rows regardless of pagination.

## Not yet shipped

- `GET /api/v1/search/commits` — commit message search; requires a
  per-repo commit FTS index that doesn't exist yet.
- `GET /api/v1/search/users` — backing query exists; REST surface
  pending; will land alongside §7 orgs/users follow-up.
- `sort=` / `order=` — every endpoint currently sorts by FTS rank;
  exposing alternative sorts (created, updated, stars) is queued.
