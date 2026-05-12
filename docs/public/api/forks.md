# Forks

Manage a repo's fork network. Mirrors GitHub's
`/repos/{owner}/{repo}/forks` shape.

Scopes: `repo:read` on GET, `repo:write` on POST. Policy gates
are `ActionRepoRead` and `ActionForkCreate`. See
[common API conventions](overview.md) for envelopes and headers.

## List forks

```
GET /api/v1/repos/{owner}/{repo}/forks[?page=&per_page=]
```

Paginated, recency-sorted, soft-deleted rows excluded. Per-row
visibility filter applies — a private fork of a public repo
only surfaces to viewers who can see the fork.

```json
[
  {
    "id":         101,
    "name":       "demo",
    "owner_login":         "bob",
    "owner_display_name":  "Bob",
    "description":         "",
    "visibility":          "public",
    "star_count":          0,
    "fork_count":          0,
    "init_status":         "initialized",
    "created_at":          "2026-05-12T15:00:00Z"
  }
]
```

`init_status` is one of `init_pending` / `initialized` /
`init_failed` — clients can poll the single-repo endpoint to
watch a fresh fork transition out of `init_pending`.

## Create a fork

```http
POST /api/v1/repos/alice/demo/forks
Authorization: Bearer <pat>
Content-Type: application/json

{
  "name":       "demo-fork",
  "visibility": "private"
}
```

All fields are optional:

- `name` — repo name on the caller's account. Defaults to the
  source repo's name. Forking your own repo into the same name
  is refused (409); pass a different `name`.
- `visibility` — must be ≤ source visibility. A public source
  can fork to private; a private source is pinned to private.

Targets the **authenticated user's namespace only** today. Org
targets land in a follow-up.

Returns `201 Created` with the persisted row immediately. The
on-disk `git clone --bare --shared` runs in the background
worker (`repo:fork_clone` kind), so the response carries
`init_status: "init_pending"` and the fork's URL resolves right
away.

```json
{
  "id":             101,
  "name":           "demo-fork",
  "owner_login":    "bob",
  "visibility":     "public",
  "init_status":    "init_pending",
  "source_repo_id": 42,
  "default_branch": "trunk",
  "created_at":     "2026-05-12T15:00:00Z"
}
```

## Errors

- `404` — source repo not visible.
- `409` — self-fork with no `name` override, target name
  collides with an existing repo, or source archived.
- `422` — `visibility` exceeds source visibility.
