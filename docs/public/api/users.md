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

`GET /api/v1/user/repos` ships in S50 §2 — see the
[Repositories](repos.md) page for the full shape and a list of
related endpoints.

## Email addresses

### List the authenticated user's emails

```
GET /api/v1/user/emails
```

Required scope: `user:read`.

Returns every email address attached to the authenticating user.
Use `?verified=true` (or `?verified=false`) to filter to a single
verification state. Omit the query parameter to list both.

#### Response

```json
[
  {
    "id": 7,
    "email": "alice+primary@example.test",
    "primary": true,
    "verified": true
  },
  {
    "id": 8,
    "email": "alice+work@example.test",
    "primary": false,
    "verified": false
  }
]
```

| Field      | Type   | Notes                                             |
|------------|--------|---------------------------------------------------|
| `id`       | int64  | Stable per-row id.                                |
| `email`    | string | The email address as stored.                      |
| `primary`  | bool   | Marks the account's primary delivery address.     |
| `verified` | bool   | `true` once a verification link has been clicked. |

#### Errors

| Status | When                                |
|-------:|-------------------------------------|
|    401 | PAT missing/invalid/expired/revoked. |
|    403 | PAT lacks `user:read` scope.         |

> **Planned.** `POST` and `DELETE /api/v1/user/emails` are not
> shipped yet — use `/settings/account` in the web UI to add or
> remove addresses for now.

## SSH keys

SSH authentication keys for git-over-SSH. Signing keys live at a
separate `/user/ssh_signing_keys` surface (not yet shipped).

### List SSH keys

```
GET /api/v1/user/keys
```

Required scope: `user:read`.

Returns the authenticated user's authentication keys (signing
keys are not returned here). Paginated via `?per_page=` and
`?page=`; the response carries the standard `Link:` header.

#### Response

```json
[
  {
    "id": 12,
    "title": "laptop",
    "key": "ssh-ed25519 AAAA…",
    "fingerprint": "SHA256:abc…",
    "key_type": "ssh-ed25519",
    "verified": true,
    "read_only": false,
    "created_at": "2026-05-12T04:00:00Z"
  }
]
```

### Get a single SSH key

```
GET /api/v1/user/keys/{id}
```

Required scope: `user:read`.

404 when the id does not belong to the authenticating user, or
when it is a signing key (use the signing-key surface).

### Add an SSH key

```
POST /api/v1/user/keys
```

Required scope: `user:write`.

#### Request body

```json
{ "title": "laptop", "key": "ssh-ed25519 AAAA…" }
```

| Field   | Type   | Notes                                                                    |
|---------|--------|--------------------------------------------------------------------------|
| `title` | string | 1–80 characters; user-visible label.                                     |
| `key`   | string | The contents of your `.pub` file (no leading comment, no trailing data). |

Returns `201` on success with the same shape as the list endpoint.

#### Errors

| Status | When                                                                         |
|-------:|------------------------------------------------------------------------------|
|    401 | PAT missing/invalid.                                                         |
|    403 | PAT lacks `user:write` scope.                                                |
|    422 | Title invalid, blob unparseable, key already registered, or per-user cap hit. |

### Delete an SSH key

```
DELETE /api/v1/user/keys/{id}
```

Required scope: `user:write`.

Returns `204 No Content` on success, `404` when the id does not
belong to the authenticating user.

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
