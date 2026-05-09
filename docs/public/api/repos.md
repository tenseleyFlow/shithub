# Repositories

> **Planned.** The repository CRUD API surface is not yet
> shipped. Today the repo lifecycle is driven through the web UI
> and the `shithubd` CLI. The shape below is the **planned** v1
> surface; it will land in a follow-up sprint.

## Planned routes

| Method | Path                                        | Scope        | Purpose                       |
|--------|---------------------------------------------|--------------|-------------------------------|
| GET    | `/api/v1/repos/{owner}/{repo}`              | `repo:read`  | Get one repo.                 |
| POST   | `/api/v1/user/repos`                        | `repo`       | Create a repo under the user. |
| POST   | `/api/v1/orgs/{org}/repos`                  | `repo,admin:org` | Create a repo under an org. |
| PATCH  | `/api/v1/repos/{owner}/{repo}`              | `repo`       | Edit settings.                |
| DELETE | `/api/v1/repos/{owner}/{repo}`              | `repo`       | Delete (with confirmation).   |
| GET    | `/api/v1/repos/{owner}/{repo}/branches`     | `repo:read`  | List branches.                |
| GET    | `/api/v1/repos/{owner}/{repo}/branches/{branch}/protection` | `repo:read` | Get branch protection rules. |
| PUT    | `/api/v1/repos/{owner}/{repo}/branches/{branch}/protection` | `repo`     | Replace branch protection.   |

The web UI's behavior is the de-facto specification — when these
endpoints land, they implement the same checks (visibility, name
collisions, owner-permissions, soft-delete grace) that the web
forms enforce.

## Today

Until the API ships, scripted repo lifecycle is best done via
the `shithubd` operator CLI on the host (operator-only) or
through HTML form submission with a session cookie (per-user but
not API-shaped).
