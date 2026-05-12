# Repositories

The repository core REST surface. Mirrors GitHub's shape where
the model matches; deviations are called out inline. List routes
emit the standard `Link:` pagination header (see [overview](overview.md)).

## Repo shape

Every list and single-repo response renders the same envelope:

```json
{
  "id": 12,
  "name": "demo",
  "full_name": "alice/demo",
  "owner_login": "alice",
  "owner_type": "user",
  "description": "first cut",
  "visibility": "public",
  "private": false,
  "default_branch": "trunk",
  "fork": false,
  "archived": false,
  "has_issues": true,
  "has_pulls": true,
  "star_count": 0,
  "watcher_count": 0,
  "fork_count": 0,
  "created_at": "2026-05-12T04:00:00Z",
  "updated_at": "2026-05-12T04:00:00Z"
}
```

`owner_type` is `"user"` or `"org"`. `default_branch` is
shithub's own default name (`trunk`), not GitHub's `main` — read
this field; don't hardcode.

## List the authenticated user's repos

```
GET /api/v1/user/repos
```

Required scope: `repo:read`. Returns every repo the user owns,
private included. Paginated; `?per_page=` (≤100) and `?page=`.

## List a user's public repos

```
GET /api/v1/users/{username}/repos
```

Required scope: `repo:read`. Returns the named user's public
repos. When the authenticated viewer is the named user, this
endpoint returns the same set as `GET /api/v1/user/repos`
(private included) to match the gh-shape.

## List an org's repos

```
GET /api/v1/orgs/{org}/repos
```

Required scope: `repo:read`. Org members see every repo
(visibility-aware). Non-members see only public repos.

## Get a single repo

```
GET /api/v1/repos/{owner}/{repo}
```

Required scope: `repo:read`. Returns the repo envelope above.
`404` when the caller can't see the repo (existence-leak-safe
treatment for private repos belonging to someone else).

## Create a personal repo

```
POST /api/v1/user/repos
```

Required scope: `repo:write`. Body fields (all optional except
`name`):

```json
{
  "name": "demo",
  "description": "first cut",
  "visibility": "public",
  "auto_init": true,
  "license_template": "mit",
  "gitignore_template": "Go"
}
```

| Field                | Type   | Notes                                                 |
|----------------------|--------|-------------------------------------------------------|
| `name`               | string | Lowercased, `[a-z0-9._-]`, 1–99 chars.                |
| `description`        | string | ≤350 chars.                                           |
| `visibility`         | string | `"public"` or `"private"`. Defaults to `"private"`.   |
| `private`            | bool   | gh-compatible alternative to `visibility`.            |
| `auto_init`          | bool   | Seed an initial README commit.                        |
| `license_template`   | string | License key (e.g. `mit`); requires `auto_init=true`.  |
| `gitignore_template` | string | Gitignore preset name; requires `auto_init=true`.     |

Returns `201` with the repo envelope.

### Errors

| Status | When                                                                  |
|-------:|-----------------------------------------------------------------------|
|    401 | PAT missing/invalid.                                                   |
|    403 | PAT lacks `repo:write` scope.                                         |
|    409 | Name already taken for this owner.                                    |
|    422 | Invalid name, description too long, unknown license/gitignore template, or actor lacks a verified primary email. |

## Create an org repo

```
POST /api/v1/orgs/{org}/repos
```

Required scope: `repo:write`. Same body shape as the personal
variant. Org members can create only when the org has
`allow_member_repo_create` enabled; owners (and site admins)
bypass that gate. Non-members get `404` (existence-leak-safe).

## Update repo settings

```
PATCH /api/v1/repos/{owner}/{repo}
```

Required scope: `repo:write`. Only the fields you send are
modified; everything else stays as it was.

```json
{
  "description": "new copy",
  "has_issues": false,
  "has_pulls": true,
  "archived": true,
  "visibility": "private"
}
```

Setting `archived` flips the archived flag through the lifecycle
orchestrator (so the on-disk repo and audit-log entry stay in
sync with the HTML surface). Visibility changes also go through
the orchestrator and emit the usual audit entry.

## Soft-delete a repo

```
DELETE /api/v1/repos/{owner}/{repo}
```

Required scope: `repo:write`. The repo is soft-deleted (matches
the web UI's deletion semantics — there is a grace window during
which a site admin can restore it). Returns `204` on success;
`404` for cross-user attempts and for already-deleted repos.

## Not yet shipped

The §2 batch covered core CRUD. The following routes are
**planned** and will land in later batches:

| Method | Path                                                  | Notes                                |
|--------|-------------------------------------------------------|--------------------------------------|
| POST   | `/api/v1/repos/{owner}/{repo}/forks`                  | Paired with shithub-cli's fork flow. |
| POST   | `/api/v1/repos/{owner}/{repo}/merge-upstream`         | Fork sync.                           |
| PUT    | `/api/v1/repos/{owner}/{repo}/topics`                 | Topic replace.                       |
| GET    | `/api/v1/repos/{owner}/{repo}/readme`                 | README content by ref.               |
| POST   | `/api/v1/repos/{template_owner}/{template_repo}/generate` | Create from template.            |
| POST   | `/api/v1/repos/{owner}/{repo}/transfer`               | Owner transfer ack.                  |
