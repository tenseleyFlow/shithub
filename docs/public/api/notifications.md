# Notifications

User-scoped inbox for the authenticated PAT. Mirrors GitHub's
`/notifications` API; each "thread" in the GitHub sense maps to
one row in shithub's `notifications` table.

Scopes:

- `user:read` — list and fetch
- `user:write` — mark read/unread, mark all read

All endpoints are implicitly scoped to the authenticated user.
There is no admin-level "read another user's inbox" surface.

## List notifications

```
GET /api/v1/notifications[?all=true|false&page=&per_page=]
```

- `all` (default `false`) returns **unread only**. Pass
  `all=true` to include read notifications.
- Standard `page` / `per_page` pagination (per_page ≤ 100,
  default 30). `Link:` headers emit `first`/`prev`/`next`/`last`.
- Sorted by `last_event_at DESC`.

```json
[
  {
    "id":            5142,
    "unread":        true,
    "reason":        "mention",
    "kind":          "issue_comment",
    "updated_at":    "2026-05-12T15:00:00Z",
    "last_event_at": "2026-05-12T15:00:00Z",
    "subject": {
      "title":  "fix race in fanout worker",
      "type":   "issue",
      "number": 42
    },
    "repository": {
      "owner_login": "alice",
      "name":        "demo",
      "full_name":   "alice/demo"
    }
  }
]
```

`subject.type` is one of `issue` / `pull_request`, or empty for
threadless system notifications.

## Get a single notification

```
GET /api/v1/notifications/threads/{id}
```

Cross-user probes return 404 (existence-leak guard) so a caller
can't enumerate other users' notification ids.

## Mark a notification read / unread

```http
PATCH /api/v1/notifications/threads/5142
Content-Type: application/json

{ "unread": false }
```

- Empty body or `unread: false` → mark **read** (gh's default).
- `unread: true` → flip back to unread (shithub extension —
  useful for "I'll come back to this").

Returns `204 No Content`.

## Mark all read

```
PUT /api/v1/notifications
```

Returns `204 No Content` once every unread row for the caller
has been flipped to read. Idempotent.

## Errors

- `404` — notification id doesn't exist, or belongs to another
  user.
- `403` — PAT lacks the required scope.
