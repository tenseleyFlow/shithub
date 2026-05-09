# Issues

> **Planned.** Issues over the API are not yet shipped. The web
> UI is the only authoring surface today.

## Planned routes

| Method | Path                                                   | Scope        |
|--------|--------------------------------------------------------|--------------|
| GET    | `/api/v1/repos/{owner}/{repo}/issues`                  | `repo:read`  |
| GET    | `/api/v1/repos/{owner}/{repo}/issues/{number}`         | `repo:read`  |
| POST   | `/api/v1/repos/{owner}/{repo}/issues`                  | `repo`       |
| PATCH  | `/api/v1/repos/{owner}/{repo}/issues/{number}`         | `repo`       |
| GET    | `/api/v1/repos/{owner}/{repo}/issues/{number}/comments`| `repo:read`  |
| POST   | `/api/v1/repos/{owner}/{repo}/issues/{number}/comments`| `repo`       |
| PATCH  | `/api/v1/repos/{owner}/{repo}/issues/comments/{id}`    | `repo`       |
| DELETE | `/api/v1/repos/{owner}/{repo}/issues/comments/{id}`    | `repo`       |

Filters on the list endpoint will mirror the web filters
(`state`, `author`, `assignee`, `label`, `milestone`, `since`,
`sort`, `direction`).

## Markdown rendering

Posted bodies are stored as raw markdown. Rendering happens at
read time, with the same `internal/markdown` pipeline the web UI
uses, so an API consumer sees the same HTML the browser would.
