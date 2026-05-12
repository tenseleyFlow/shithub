# Followers / following

Mirrors GitHub's followers REST surface: list followers and
following for any user, plus the per-actor follow / unfollow
edge management.

Scopes:

- `user:read` on the GETs
- `user:write` on PUT/DELETE (mutating the authenticated user's
  follow edges)

## Endpoints

```
GET    /api/v1/users/{username}/followers              followers list (paginated)
GET    /api/v1/users/{username}/following              following list (paginated)
GET    /api/v1/user/following/{target}                 204 / 404 membership probe
PUT    /api/v1/user/following/{target}                 follow
DELETE /api/v1/user/following/{target}                 unfollow
```

Note: shithub's follow model also lets users follow **orgs**.
This REST surface only exposes user→user follows; the org-follow
variants stay on the HTML profile pages for now (post-MVP
roadmap item).

## List shapes

```json
[
  {
    "user_id":      42,
    "username":     "bob",
    "display_name": "Bob",
    "followed_at":  "2026-05-12T18:00:00Z"
  }
]
```

Paginated with `Link:` headers (default per_page 30, max 100).

## Membership probe

```
GET /api/v1/user/following/bob
```

Returns `204 No Content` when the authenticated user follows
`bob`, `404 Not Found` otherwise. No body either way — clients
inspect the status code directly (matches gh).

## Follow / unfollow

```
PUT    /api/v1/user/following/bob
DELETE /api/v1/user/following/bob
```

Both return `204 No Content` and are idempotent — re-following or
re-unfollowing has no side effect beyond emitting the audit row.
The follow side fires a public `followed_user` domain event for
activity-feed surfaces.

## Errors

- `404` — target username doesn't exist (uniform envelope; can't
  enumerate via response shape).
- `422` — `target` equals the authenticated user (cannot follow
  yourself).
- `429` — per-user follow/unfollow rate cap exceeded
  (200 events/hour by default).
- `403` — PAT lacks the required scope.
