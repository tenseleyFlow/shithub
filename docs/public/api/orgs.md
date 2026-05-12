# Organizations

Org-level metadata and membership graph. Org creation is web-only
in v1 (the `/orgs/new` form); the REST surface is read-side until
the CLI gains an org-create flow.

## Org shape

```json
{
  "id": 42,
  "slug": "acme",
  "login": "acme",
  "display_name": "Acme",
  "description": "Acme org",
  "location": "",
  "website": "",
  "plan": "free",
  "allow_member_repo_create": true,
  "created_at": "2026-05-12T05:00:00Z"
}
```

`login` is a gh-compatible alias for `slug`.

## List the authenticated user's orgs

```
GET /api/v1/user/orgs
```

Required scope: `user:read`. Returns the caller's full membership
list (with role) sorted by org slug.

```json
[
  {
    "org_id": 42,
    "slug": "acme",
    "login": "acme",
    "display_name": "Acme",
    "role": "owner"
  }
]
```

## List a user's orgs

```
GET /api/v1/users/{username}/orgs
```

Required scope: `user:read`. shithub does not yet distinguish
public/private membership, so this endpoint returns the same set
the named user sees on their own `/user/orgs`. `404` for unknown
usernames.

## Get a single org

```
GET /api/v1/orgs/{org}
```

Required scope: `user:read`. `404` for unknown slugs and
soft-deleted orgs.

## List org members

```
GET /api/v1/orgs/{org}/members
```

Required scope: `user:read`. Sorted by role (owner first) then
username.

```json
[
  {
    "user_id": 7,
    "username": "alice",
    "display_name": "Alice",
    "role": "owner",
    "joined_at": "2026-05-12T05:00:00Z"
  }
]
```

## Not yet shipped

- Teams REST surface.
- Invitations (accept / decline / cancel).
- Org-level settings and billing plan transitions.
- Member role changes through the API.
