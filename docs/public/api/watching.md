# Watching / subscriptions

Mirrors GitHub's `/repos/{owner}/{repo}/subscription` family —
manage the authenticated user's watch level on a repo.

shithub's internal vocabulary is **watch**; the public URL uses
the gh-flavored noun **subscription** so existing CLI ports
keep working without churn.

Scopes:

- `repo:read` on GETs
- `user:write` on PUT/DELETE (watching is a user-scoped
  preference, not a repo mutation)

## Watch levels

| Level | Meaning |
|-------|---------|
| `all` | Notify on every event in the repo. |
| `participating` | Notify only when @-mentioned or directly involved (the **implicit default**; matches gh's "Participating and @mentions"). |
| `ignore` | Mute. Excluded from subscriber lists. |

## Endpoints

```
GET    /api/v1/repos/{owner}/{repo}/subscribers          paginated list of watchers
GET    /api/v1/repos/{owner}/{repo}/subscription         viewer's current watch level
PUT    /api/v1/repos/{owner}/{repo}/subscription         set an explicit level
DELETE /api/v1/repos/{owner}/{repo}/subscription         revert to the implicit default
```

### List subscribers

```
GET /api/v1/repos/alice/demo/subscribers[?page=&per_page=]
```

Paginated, recency-sorted, **excludes** users at `ignore` level
and suspended users.

```json
[
  {
    "user_id":      42,
    "username":     "bob",
    "display_name": "Bob",
    "level":        "all",
    "updated_at":   "2026-05-12T15:00:00Z"
  }
]
```

### Get your subscription

```
GET /api/v1/repos/alice/demo/subscription
```

```json
{ "level": "participating", "explicit": false }
```

`explicit: false` means the caller hasn't set an explicit watch
row — they're on the implicit `participating` default. After a
PUT, follow-up GETs return `explicit: true`.

### Set / clear your subscription

```http
PUT /api/v1/repos/alice/demo/subscription
Content-Type: application/json

{ "level": "all" }
```

```
DELETE /api/v1/repos/alice/demo/subscription
```

DELETE removes the explicit row and reverts to the implicit
default; returns `204 No Content`. PUT returns `200` with the
new subscription shape.

## Errors

- `404` — repo not visible to the caller.
- `403` — PAT lacks the required scope.
- `422` — `level` is not one of `all` / `participating` /
  `ignore`.
