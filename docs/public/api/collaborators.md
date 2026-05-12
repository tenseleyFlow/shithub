# Repo collaborators

Mirrors GitHub's `/repos/{owner}/{repo}/collaborators` family of
endpoints — manage the per-repo role grants that policy.Can
consults for non-owner callers.

All endpoints share the canonical [API conventions](overview.md)
(JSON error envelopes, `X-RateLimit-*`, `Link:` pagination on
the list endpoint).

## Auth

| Method | Required scope | Policy gate |
|--------|---------------|-------------|
| GET    | `repo:read`   | `ActionRepoRead` |
| PUT    | `repo:write`  | `ActionRepoAdmin` |
| DELETE | `repo:write`  | `ActionRepoAdmin` |

Mutations are restricted to repo owners and `admin`-role
collaborators. shithub doesn't currently mint a separate
`repo:admin` scope; the per-route policy check enforces the
admin floor on top of the broader `repo:write` scope.

## Endpoints

```
GET    /api/v1/repos/{owner}/{repo}/collaborators
GET    /api/v1/repos/{owner}/{repo}/collaborators/{username}
GET    /api/v1/repos/{owner}/{repo}/collaborators/{username}/permission
PUT    /api/v1/repos/{owner}/{repo}/collaborators/{username}
DELETE /api/v1/repos/{owner}/{repo}/collaborators/{username}
```

### List

Returns every direct collaborator row for the repo (org owner
membership is exposed separately under `/orgs/{org}/members`).

```json
[
  {
    "user_id":      42,
    "username":     "bob",
    "display_name": "Bob",
    "role":         "write"
  }
]
```

### Membership probe

`GET .../collaborators/{username}` returns `204 No Content`
when the user is a collaborator, `404` otherwise. No body —
clients check the status code directly. Mirrors GitHub's
behaviour.

### Permission level

```json
{ "user": "bob", "permission": "write" }
```

`permission` is one of `read` / `triage` / `write` / `maintain`
/ `admin`, or `"none"` when the user is not a collaborator.

### Add or upgrade

```http
PUT /api/v1/repos/alice/demo/collaborators/bob
Content-Type: application/json

{ "role": "write" }
```

`role` accepts the shithub canonical names plus the GitHub-style
aliases (`pull` → `read`, `push` → `write`). Returns the
upgraded row at 200. Refuses (422) to enrol the repo owner — an
owner already has full implicit access, so a collaborator row
would only confuse the policy decision.

### Remove

```
DELETE /api/v1/repos/alice/demo/collaborators/bob
```

Returns `204`. Idempotent — removing a non-collaborator still
returns 204 (matches gh).
