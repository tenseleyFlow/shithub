# Search

> **Planned.** Search over the API is not shipped yet. The web UI's
> `/search` is the only entry point today.

## Planned routes

| Method | Path                              | Scope                          |
|--------|-----------------------------------|--------------------------------|
| GET    | `/api/v1/search/code`             | None (public corpus only without `repo:read`); `repo:read` to include private. |
| GET    | `/api/v1/search/repositories`     | Same scoping.                  |
| GET    | `/api/v1/search/issues`           | Same scoping.                  |
| GET    | `/api/v1/search/users`            | None.                          |

Query parameters:

- `q=` — search query, with the same operators as
  [Search (user docs)](../user/search.md).
- `sort=` — `created`, `updated`, `stars`, `relevance` (default).
- `order=` — `asc`, `desc`.
- `per_page=`, `cursor=`.

Response shape (proposed):

```json
{
  "total_count": 142,
  "items": [
    { /* type-dependent record */ }
  ]
}
```

The total is **counted to a cap** (10000) — for larger result
sets the API returns "10000+" semantics rather than counting
exactly. Consumers that need totals should narrow the query.

## Visibility

Search results are filtered by the requesting PAT's user's
visibility, identical to the web UI's behavior. A token cannot
see a result it would not see in a browser logged in as the
same user.
