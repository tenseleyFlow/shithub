# Repository follow-ups — README + topics + merge-upstream

Three additive endpoints that round out the §2 repos surface. The
core repos CRUD (list/single/create/patch/delete) lives in
[Repositories](./repos.md); this page covers the three that arrived
later: rendering-free README fetch, topic replacement, and fork
sync.

Scopes:

- `repo:read` on README.
- `repo:write` on topics replace/clear and merge-upstream.

## Endpoints

```
GET    /api/v1/repos/{o}/{r}/readme[?ref=]
PUT    /api/v1/repos/{o}/{r}/topics
DELETE /api/v1/repos/{o}/{r}/topics
POST   /api/v1/repos/{o}/{r}/merge-upstream
```

## README

`GET /api/v1/repos/{o}/{r}/readme` returns the repo's README at
`ref` (default branch when omitted). The handler walks the root
tree, picks the first entry whose name starts with `readme`
(case-insensitive), and prefers `.md` / `.markdown` over plain
text so a repo with both `README.md` and `README.rst` returns the
markdown variant — matching the HTML code-view's choice.

Response shape:

```json
{
  "name":         "README.md",
  "path":         "README.md",
  "size":         312,
  "encoding":     "base64",
  "content":      "IyBEZW1vIHJlcG8K...",
  "download_url": "http://shithub.local/alice/demo/raw/trunk/README.md"
}
```

`content` is base64-encoded so binary or UTF-16 READMEs round-trip
cleanly. `size` matches the decoded byte count. The handler caps
reads at 1 MiB (matching the HTML render cap); larger READMEs are
truncated to that cap but the response still succeeds. Use
`download_url` to stream the full blob when the cap matters.

Errors:

| Status | Cause                                                    |
|------:|-----------------------------------------------------------|
| 404   | Repo missing, caller lacks read, ref absent, or no README |

The 404 is existence-leak-safe: a private repo the caller can't
read returns the same 404 as a public repo with no README.

## Topics

`PUT /api/v1/repos/{o}/{r}/topics` replaces the full topic set
atomically. Body:

```json
{ "names": ["go", "rest-api", "shithub"] }
```

Response (200):

```json
{ "names": ["go", "rest-api", "shithub"] }
```

Topics are normalized server-side (lowercased, deduped) and
validated:

- Max 20 per repo.
- Each name 1–50 chars, lowercase letters / digits / hyphens
  only.

Invalid input returns `422` with a JSON error describing the
violated constraint.

`DELETE /api/v1/repos/{o}/{r}/topics` clears all topics. Returns
`204 No Content`. Idempotent — clearing an empty set still
returns 204.

## Merge-upstream (fork sync)

`POST /api/v1/repos/{o}/{r}/merge-upstream` fast-forwards a fork's
default branch to its upstream. Mirrors GitHub's "Sync fork"
button.

The handler refuses non-forks with `422`. For a real fork it
calls the shared `fork.Sync` orchestrator, which only proceeds
when the merge is a clean fast-forward — divergent forks return
`409` and must be reconciled by the user via their git client.

Successful response:

```json
{
  "merged":      true,
  "old_oid":     "a1b2...",
  "new_oid":     "c3d4...",
  "base_branch": "trunk",
  "message":     "fast-forwarded to upstream"
}
```

Already-up-to-date (200, not an error):

```json
{
  "merged":  false,
  "message": "already up to date"
}
```

Errors:

| Status | Cause                                                    |
|------:|-----------------------------------------------------------|
| 409   | Fork has diverged from upstream — sync via your client.  |
| 409   | Ref changed concurrently — retry.                        |
| 409   | Fork still being initialized — retry shortly.            |
| 422   | Repo is not a fork.                                      |
| 422   | Source or fork default branch is empty.                  |

The endpoint is intentionally narrower than GitHub's: we only
fast-forward. A "fork sync with merge commit" mode would require
the runner to pick an author identity and resolve conflicts, both
of which we'd rather the caller do locally.
