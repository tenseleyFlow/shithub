# Pull requests

> **Planned.** Pull request endpoints are not yet shipped.

## Planned routes

| Method | Path                                                     | Scope        |
|--------|----------------------------------------------------------|--------------|
| GET    | `/api/v1/repos/{owner}/{repo}/pulls`                     | `repo:read`  |
| GET    | `/api/v1/repos/{owner}/{repo}/pulls/{number}`            | `repo:read`  |
| POST   | `/api/v1/repos/{owner}/{repo}/pulls`                     | `repo`       |
| PATCH  | `/api/v1/repos/{owner}/{repo}/pulls/{number}`            | `repo`       |
| GET    | `/api/v1/repos/{owner}/{repo}/pulls/{number}/files`      | `repo:read`  |
| GET    | `/api/v1/repos/{owner}/{repo}/pulls/{number}/commits`    | `repo:read`  |
| GET    | `/api/v1/repos/{owner}/{repo}/pulls/{number}/reviews`    | `repo:read`  |
| POST   | `/api/v1/repos/{owner}/{repo}/pulls/{number}/reviews`    | `repo`       |
| PUT    | `/api/v1/repos/{owner}/{repo}/pulls/{number}/merge`      | `repo`       |
| GET    | `/api/v1/repos/{owner}/{repo}/pulls/{number}/comments`   | `repo:read`  |
| POST   | `/api/v1/repos/{owner}/{repo}/pulls/{number}/comments`   | `repo`       |

The merge endpoint is gated by branch protection: status checks,
required reviewers, and conversation-resolution rules apply
identically to API-driven and UI-driven merges.
