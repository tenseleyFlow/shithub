# API overview

shithub exposes a small REST API at `/api/v1/`. The user-facing API is
PAT-authenticated, JSON-bodied, and CSRF-exempt. The Actions runner API
under the same prefix uses runner registration tokens plus per-job JWTs
instead of PATs.

> **Status.** The API is intentionally narrow today. Endpoints
> currently shipped: `GET /api/v1/user`, the
> `/api/v1/repos/{owner}/{repo}/check-runs` family, and the
> `/api/v1/user/starred*` stars endpoints, plus the Actions lifecycle
> routes in [Actions workflow API](actions.md). Other sections of this
> reference (Issues, Pull requests, Webhooks, etc.) describe the
> **planned** shape and will land in subsequent sprints. Pages
> that document planned-only endpoints carry a banner.

## Authentication

Every API request requires a personal access token. See
[Personal access tokens](../user/personal-access-tokens.md) for
how to create one. Runner endpoints documented in
[Actions runner API](actions-runner.md) are the exception; they reject
PATs and require runner credentials.

```
Authorization: Bearer shp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

`Authorization: token shp_…` is also accepted as a synonym.

A request with no `Authorization` header — or with an invalid /
expired / revoked PAT — returns `401 Unauthorized` with the
canonical JSON error envelope. The response also carries a
`WWW-Authenticate: Bearer realm="shithub", error="invalid_token"`
challenge so HTTP-aware clients can reauthenticate cleanly.

```json
{"error": "invalid token"}
```

A request whose PAT lacks the scope a route requires returns
`403 Forbidden` with the required scope surfaced in the
`X-Accepted-OAuth-Scopes` response header. The token's actual
scopes are echoed on every PAT-authenticated response (including
errors) via `X-OAuth-Scopes`, so clients can build precise
"missing scope X, present scopes Y, Z" error messages without
parsing the body:

```
HTTP/1.1 403 Forbidden
X-OAuth-Scopes: user:read
X-Accepted-OAuth-Scopes: repo:write
Content-Type: application/json; charset=utf-8

{"error":"token lacks required scope: repo:write"}
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
  max 100) and return an RFC 8288 `Link:` header with `next`,
  `prev`, `first`, `last` URLs. URLs are absolute and use the
  instance's configured public base URL. Forward-only feeds emit
  only `next` / `prev` (no `first` / `last`) when totals are
  expensive to compute. Cursor pagination on hot lists uses
  `?cursor=…` and returns the next cursor in the same `Link:`
  header.
- **Body cap:** request bodies are capped at 256 KiB. Larger
  payloads return `413`.
- **Rate limits:** every response includes `X-RateLimit-Limit`,
  `X-RateLimit-Remaining`, and `X-RateLimit-Reset` headers (Unix
  timestamp). Exceeding the limit returns `429` with the canonical
  error envelope, a `Retry-After` header (seconds), and
  `X-RateLimit-Remaining: 0`. Defaults: 5000 / hour for PAT
  callers (keyed by token id) and 60 / hour for anonymous callers
  (keyed by remote IP). Operators tune via
  `SHITHUB_RATELIMIT__API__AUTHED_PER_HOUR` and
  `SHITHUB_RATELIMIT__API__ANON_PER_HOUR`.
- **Request id:** every response echoes `X-Request-Id`. If the
  caller sends one (matching `[a-z0-9/:_-]{1,128}`) it's reused;
  otherwise the server generates a fresh 16-byte hex id. Quote
  this id in support reports — it correlates against the server
  logs.

## Capability discovery

`GET /api/v1/meta` is unauthenticated and returns the server's
version stamp plus a list of feature capability strings. Clients
use it to gate optional features without trial-and-error:

```json
{
  "version": "v0.2.0",
  "commit": "abc1234",
  "built_at": "2026-05-12T03:00:00Z",
  "capabilities": ["pat-auth", "check-runs", "stars", "actions-lifecycle"]
}
```

New capability identifiers are appended as feature batches land;
existing entries are stable.

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
