# Actions caches

Read + delete surface over the `workflow_caches` table —
shithub's record of cache tarballs uploaded by `actions/cache@v*`
workflow steps.

The runner-side cache upload protocol that POPULATES the table is
a separate sprint; this REST surface lands first so operators have
an audit + purge seat for when caches arrive.

Scopes:

- `repo:read` on list
- `repo:write` on delete

## Endpoints

```
GET    /api/v1/repos/{owner}/{repo}/actions/caches
DELETE /api/v1/repos/{owner}/{repo}/actions/caches
DELETE /api/v1/repos/{owner}/{repo}/actions/caches/{cache_id}
```

`GET` accepts `?key=&ref=&page=&per_page=`. `DELETE` without a
`{cache_id}` deletes by `?key=...` and optional `&ref=...`.

## List response

```json
{
  "total_count": 2,
  "actions_caches": [
    {
      "id":               17,
      "key":              "node-modules",
      "version":          "8f3b...",
      "ref":              "refs/heads/trunk",
      "size_bytes":       1048576,
      "last_accessed_at": "2026-05-12T18:00:00Z",
      "created_at":       "2026-05-12T17:00:00Z"
    }
  ]
}
```

Sorted by `last_accessed_at DESC` so an operator sees the live
caches first. Standard `Link:` pagination headers are emitted
when results exceed `per_page` (default 30, max 100).

Filter params:

- `key=<cache_key>` — restrict to a single cache key.
- `ref=<git_ref>` — restrict to caches created against this ref
  (e.g. `refs/heads/trunk`).

## Delete by id

```
DELETE /api/v1/repos/alice/demo/actions/caches/17
```

Returns `204 No Content`. The DB row is removed atomically; the
tarball in object storage is purged best-effort in the background.

`404` when:

- the cache id is unknown
- the cache exists but belongs to a different repo (existence-leak-safe)
- the caller lacks `repo:write` on the target (returned as 404 by
  the policy gate, not 403, to keep the existence-leak guarantee)

## Delete by key

```
DELETE /api/v1/repos/alice/demo/actions/caches?key=node-modules
```

Removes every cache with that key, regardless of version, scoped
to the repo. Add `ref=...` to scope by ref as well. Returns `204`
even when zero rows match (idempotent).

`400` when the `key` query parameter is missing or empty.

## Errors

| Status | Cause                                                |
|------:|--------------------------------------------------------|
| 400   | DELETE-by-key without a `key` query parameter.        |
| 403   | PAT lacks `repo:write` on the delete endpoints.       |
| 404   | Cache id unknown or belongs to a different repo.      |
