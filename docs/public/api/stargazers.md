# Stargazers / starred lists

Read-only directory pages that mirror GitHub's stargazer surfaces:
list the users who starred a repo, or list the repos a user has
starred. The existing `/api/v1/user/starred` + per-repo PUT/DELETE
routes (the caller-self star management surface) are documented
separately in [`users.md`](./users.md).

Scopes:

- `repo:read` on the per-repo stargazer list
- `user:read` on the per-user starred list

## Endpoints

```
GET /api/v1/repos/{owner}/{repo}/stargazers     users who starred this repo
GET /api/v1/users/{username}/starred             repos this user has starred
```

Both endpoints are paginated with the standard `Link:` headers
(default per_page 30, max 100) and recency-sorted (newest star
first).

## Stargazers list

```
GET /api/v1/repos/alice/demo/stargazers
```

```json
[
  {
    "user_id":      42,
    "username":     "bob",
    "display_name": "Bob",
    "starred_at":   "2026-05-12T18:00:00Z"
  }
]
```

Visibility follows the repo: a private-repo stargazer list is only
visible to callers with `repo:read` access to that repo. Cross-actor
attempts return `404` to avoid leaking existence.

## User starred list

```
GET /api/v1/users/alice/starred
```

```json
[
  {
    "repo_id":          17,
    "owner":            "alice",
    "name":             "demo",
    "description":      "demo repo",
    "visibility":       "public",
    "star_count":       3,
    "primary_language": "Go",
    "updated_at":       "2026-05-12T17:00:00Z",
    "starred_at":       "2026-05-12T18:00:00Z"
  }
]
```

For cross-user views the list is post-filtered: repos the caller
can't read (private repos they're not a collaborator on) are
silently dropped from the response. The total reflected in the
`Link:` header reflects the raw star count for the user; the
returned page may therefore contain fewer items than `per_page`
when private stars are filtered out.

## Errors

- `404` — repo or username doesn't exist (uniform envelope; can't
  enumerate via response shape).
- `403` — PAT lacks the required scope.
