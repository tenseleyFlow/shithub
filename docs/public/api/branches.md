# Branches and tags

Read-only enumeration of a repo's git refs. Mirrors GitHub's
`/repos/{owner}/{repo}/branches` + `/tags` endpoints with one
shithub-specific addition (`is_default` on branches).

All endpoints require an `Authorization: Bearer <pat>` header
with the `repo:read` scope and gate on `ActionRepoRead`. The
[common API conventions](overview.md) apply — JSON error
envelopes, `X-RateLimit-*` headers, `Link:` pagination.

## List branches

```
GET /api/v1/repos/{owner}/{repo}/branches[?page=&per_page=]
```

Paginated (default 30, max 100). Sorted alphabetically by ref
name.

```json
[
  {
    "name":       "trunk",
    "commit_sha": "5f3a…",
    "protected":  false,
    "is_default": true
  },
  {
    "name":       "release/v1.0",
    "commit_sha": "8c0d…",
    "protected":  true,
    "is_default": false
  }
]
```

`protected` reflects the repo's branch-protection rule set —
the longest-prefix match against the configured patterns
(`release/*`, etc.). An uninitialised / empty repo returns an
empty list rather than 404.

## Get a single branch

```
GET /api/v1/repos/{owner}/{repo}/branches/{branch}
```

`{branch}` may contain forward slashes (`feature/x`,
`release/v1.0`). The route accepts them verbatim or
URL-encoded (`feature%2Fx`). Returns the same shape as a list
entry; unknown branches `404`.

## List tags

```
GET /api/v1/repos/{owner}/{repo}/tags[?page=&per_page=]
```

```json
[
  { "name": "v0.1.0", "commit_sha": "5f3a…" },
  { "name": "v0.2.0", "commit_sha": "9d4e…" }
]
```

Sorted alphabetically. We don't currently surface annotated-tag
metadata (tagger / message) — that's a follow-up if the CLI's
release flow needs it.
