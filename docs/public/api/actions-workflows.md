# Actions workflows

Read-only access to the workflow files in a repo plus the
`workflow_dispatch` trigger that the HTML "Run workflow" button
fires. Workflow runs themselves live at the
[Actions workflow runs](./actions-runs.md) surface.

Scopes:

- `repo:read` on the list / single-workflow GETs
- `repo:write` on the dispatch POST (same trust boundary as the
  HTML UI, where any pusher can run a workflow)

## Endpoints

```
GET  /api/v1/repos/{owner}/{repo}/actions/workflows                          list discovered workflows
GET  /api/v1/repos/{owner}/{repo}/actions/workflows/{id_or_file}             single workflow
POST /api/v1/repos/{owner}/{repo}/actions/workflows/{file}/dispatches        workflow_dispatch
```

## Discovery model

shithub does not keep a `workflows` table. The list endpoint walks
`.shithub/workflows/` in the git tree at the repo's default-branch
HEAD (or `?ref=` when provided) and parses each `*.yml`/`*.yaml`
file to extract its `name:`. Files that fail to parse still appear
in the listing — with their basename as the name — so the listing
reflects ground truth rather than silently dropping broken
workflows.

The `id` field on each entry is a deterministic 64-bit hash of the
file path so gh-shaped clients that pass `workflow_id` still work:
both `ci.yml` (basename), `.shithub/workflows/ci.yml` (full path),
and the numeric id are accepted on the single-workflow GET. The
dispatch endpoint accepts only the basename or full path (the
numeric id is rejected with 400 to keep dispatch a single
round-trip; clients with only an id should hit the list first to
resolve the path).

## List response

```json
[
  {
    "id":    7142839129843290000,
    "name":  "CI",
    "path":  ".shithub/workflows/ci.yml",
    "file":  "ci.yml",
    "state": "active"
  }
]
```

`state` is always `"active"` for now — enable/disable knobs land
in a follow-up PR alongside a `workflow_disabled` table.

## Dispatch request

```
POST /api/v1/repos/alice/demo/actions/workflows/ci.yml/dispatches
Content-Type: application/json

{
  "ref":    "trunk",
  "inputs": { "env": "prod", "debug": "true" }
}
```

- `ref` defaults to the repo's default branch when omitted.
- `inputs` must match the workflow's declared
  `on.workflow_dispatch.inputs`:
  - Unknown inputs → `400 invalid_request`.
  - Required (non-boolean) inputs missing → 400.
  - Booleans must be `"true"` / `"false"` strings.
  - Choices must match one of the declared options.
- A workflow without `on.workflow_dispatch` returns 400.
- Successful dispatch returns `204 No Content`. The run shows up in
  the [actions runs](./actions-runs.md) feed shortly after the
  trigger pipeline enqueues it.

## Errors

| Status | Cause                                                       |
|------:|--------------------------------------------------------------|
| 400   | Workflow has no `on.workflow_dispatch`, or inputs malformed. |
| 400   | Numeric id passed to dispatch (use basename or full path).    |
| 403   | PAT lacks `repo:write`.                                       |
| 404   | Repo, ref, or workflow file unknown.                          |
