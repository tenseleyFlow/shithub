# Users

## Get the authenticated user

```
GET /api/v1/user
```

Required scope: `user:read`.

Returns the user record for the account that owns the
authenticating PAT.

### Response

```json
{
  "id": 42,
  "username": "alice",
  "name": "Alice Example",
  "email_verified": true,
  "created_at": "2026-05-09T16:30:00Z"
}
```

| Field            | Type    | Notes                                                |
|------------------|---------|------------------------------------------------------|
| `id`             | int64   | Stable numeric ID.                                   |
| `username`       | string  | Account username; URL-safe slug.                     |
| `name`           | string  | Display name; may be empty if not set.               |
| `email_verified` | bool    | Whether the primary email has been verified.         |
| `created_at`     | string  | RFC 3339 UTC timestamp of account creation.          |

### Errors

| Status | When                                |
|-------:|-------------------------------------|
|    401 | PAT missing/invalid/expired/revoked. |
|    403 | PAT lacks `user:read` scope.         |
|    404 | User record not found (suspended or deleted between auth and lookup). |

## Get a user by username

> **Planned.** `GET /api/v1/users/{username}` is not shipped yet.

## Update the authenticated user

> **Planned.** `PATCH /api/v1/user` is not shipped yet.

## List the authenticated user's repos

> **Planned.** `GET /api/v1/user/repos` is not shipped yet.

## Stars

The starred-repos surface for the authenticating user.

### List starred repos

```
GET /api/v1/user/starred
```

Required scope: `user:read`.

Returns the list of `(owner, repo)` pairs the user has starred,
most-recent first. Pagination via `?cursor=…` and `?per_page=`.

### Star a repo

```
PUT /api/v1/user/starred/{owner}/{repo}
```

Required scope: `user` (write).

Idempotent: starring an already-starred repo returns `204` and
does not duplicate the row. Returns `404` if the repo doesn't
exist or the user can't see it.

### Unstar a repo

```
DELETE /api/v1/user/starred/{owner}/{repo}
```

Required scope: `user` (write).

Idempotent: unstarring a not-starred repo returns `204`.
