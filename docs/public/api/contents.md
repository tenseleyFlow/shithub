# Repo contents

Read-only file/directory access. Mirrors GitHub's
`GET /repos/{owner}/{repo}/contents/{path}` — one endpoint with
two response shapes depending on whether the path resolves to
a tree (directory) or a blob (file).

Scope: `repo:read`. Policy gate: `ActionRepoRead`. See the
[common API conventions](overview.md) for JSON errors and rate
limits.

## Endpoint

```
GET /api/v1/repos/{owner}/{repo}/contents/{path}[?ref=]
```

- `path` may contain forward slashes (`src/main.go`); empty
  path returns the repo root.
- `ref` defaults to the repo's `default_branch`. Accepts a
  branch name, tag, or full / unambiguous-prefix commit SHA.

Both `/contents` (no trailing path) and `/contents/` (empty
path) map to "repo root".

The registered path-bearing route literal is
`/api/v1/repos/{owner}/{repo}/contents/*`.

## Directory response

An array of entries, **directories first**, then files sorted
alphabetically:

```json
[
  { "path": "src",         "name": "src",       "type": "dir",  "sha": "5f3a…" },
  { "path": "README.md",   "name": "README.md", "type": "file", "size": 132, "sha": "a14b…" },
  { "path": "vendor",      "name": "vendor",    "type": "submodule", "sha": "9c0d…" }
]
```

`type` is one of `dir` / `file` / `symlink` / `submodule`.
`size` is only set on `file` and `symlink` entries.

## File response

```json
{
  "path":      "README.md",
  "name":      "README.md",
  "type":      "file",
  "size":      132,
  "sha":       "a14b…",
  "encoding":  "base64",
  "content":   "IyBkZW1vCg==",
  "binary":    false,
  "truncated": false
}
```

- `content` is always base64-encoded (matches GitHub). The
  server detects UTF-8 validity and sets `binary: true` when
  the bytes aren't valid UTF-8 — clients can skip rendering
  binary blobs.
- Files larger than **1 MiB** return `truncated: true` with an
  empty `content`. Clients should follow up with the raw blob
  download path for the full bytes.

## Special types

- `symlink` entries surface their target size and SHA but no
  body — the bytes are the symlink target string, which the
  caller can fetch via the file shape if needed.
- `submodule` entries return only `path`/`name`/`type`/`sha`
  (the recorded commit). Resolving the submodule's tree is the
  caller's job.

## Errors

- `404` — path doesn't exist on the requested ref.
- `403` — caller lacks `repo:read` (PAT scope) or
  `ActionRepoRead` (policy) on a private repo.
