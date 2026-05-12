# Labels

Repo-scoped labels for issues and pull requests. The default
label set is seeded at repo create and can be edited or removed
freely.

## Label shape

```json
{
  "id": 7,
  "name": "bug",
  "color": "ee0701",
  "description": "Something isn't working",
  "created_at": "2026-05-12T05:00:00Z"
}
```

`color` is six hex chars without the leading `#`.

## List labels

```
GET /api/v1/repos/{owner}/{repo}/labels
```

Required scope: `repo:read`. Sorted alphabetically by name.

## Get a single label

```
GET /api/v1/repos/{owner}/{repo}/labels/{name}
```

Required scope: `repo:read`. `404` when the label does not exist.

## Create a label

```
POST /api/v1/repos/{owner}/{repo}/labels
```

Required scope: `repo:write`. Policy: `ActionIssueLabel`.

```json
{ "name": "needs-triage", "color": "ff00aa", "description": "triage me" }
```

| Field         | Type   | Notes                                  |
|---------------|--------|----------------------------------------|
| `name`        | string | Required, 1–50 chars.                  |
| `color`       | string | Required, six hex chars (no `#`).      |
| `description` | string | Optional, ≤100 chars.                  |

Returns `201`. `409` when the name is already taken on the repo;
`422` on invalid color or empty name.

## Update a label

```
PATCH /api/v1/repos/{owner}/{repo}/labels/{name}
```

Required scope: `repo:write`. Send only the fields you want to
change. Renaming a label is allowed via `name`.

```json
{ "color": "00ff00", "description": "ready for triage" }
```

## Delete a label

```
DELETE /api/v1/repos/{owner}/{repo}/labels/{name}
```

Required scope: `repo:write`. Returns `204`. Issue / PR label
associations cascade away automatically.
