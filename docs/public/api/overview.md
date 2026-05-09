# API overview

shithub exposes a small REST API at `/api/v1/`. It's
PAT-authenticated, JSON-bodied, and CSRF-exempt.

> **Status.** The API is intentionally narrow today. Endpoints
> currently shipped: `GET /api/v1/user`, the
> `/api/v1/repos/{owner}/{repo}/check-runs` family, and the
> `/api/v1/user/starred*` stars endpoints. Other sections of this
> reference (Issues, Pull requests, Webhooks, etc.) describe the
> **planned** shape and will land in subsequent sprints. Pages
> that document planned-only endpoints carry a banner.

## Authentication

Every API request requires a personal access token. See
[Personal access tokens](../user/personal-access-tokens.md) for
how to create one.

```
Authorization: Bearer shp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

`Authorization: token shp_…` is also accepted as a synonym.

A request with no `Authorization` header — or with an invalid /
expired / revoked PAT — returns `401 Unauthorized` with:

```json
{"error": "unauthenticated"}
```

A request whose PAT lacks the scope a route requires returns
`403 Forbidden` with:

```json
{"error": "insufficient scope"}
```

## Scopes

Every route declares the scope(s) a token must hold. See
[scopes table](../user/personal-access-tokens.md#scopes).

Scopes are **grants only** — a token cannot do something the
underlying user cannot. Holding `repo` does not let a token push
to a repo the user has no access to.

## Conventions

- **Base URL:** `https://<your-instance>/api/v1/`
- **Content type:** `application/json; charset=utf-8` for
  request bodies and responses.
- **Error responses:** `{"error": "<short message>"}` with a
  conventional HTTP status.
- **Cache-Control:** every API response sets `no-store`.
- **Pagination:** list endpoints accept `?per_page=` (default 30,
  max 100) and return a `Link:` header with `next`, `prev`,
  `first`, `last` URLs (RFC 5988). Cursor pagination on hot lists
  uses `?cursor=…` and returns the next cursor in the `Link:`
  header.
- **Body cap:** request bodies are capped at 256 KiB. Larger
  payloads return `413`.
- **Rate limits:** every response includes `X-RateLimit-Limit`,
  `X-RateLimit-Remaining`, and `X-RateLimit-Reset` headers (Unix
  timestamp). Exceeding the limit returns `429`.

## Versioning

The path prefix `/api/v1/` is the version. Backwards-incompatible
changes will land under `/api/v2/`. Additions (new endpoints, new
fields on responses) are not breaking and land under v1.

## Date format

All timestamps are RFC 3339 UTC: `2026-05-09T16:30:00Z`.

## ID stability

Numeric IDs are stable for the life of the row; reusing a name
slot doesn't reuse the ID. URLs that take a `{owner}` and `{repo}`
will redirect after a rename — but external callers should
prefer numeric IDs where the API exposes them.
