# Code tab

The code tab is the GitHub-style repo browser: tree listing, blob view
with syntax highlighting, raw view, "Go to file" finder, and the
branch/tag switcher. For populated repos, `/{owner}/{repo}` renders the
default branch Code tab directly, matching GitHub's canonical repo URL.

## Routes

| Route                                            | Handler                          |
| ------------------------------------------------ | -------------------------------- |
| `GET /{owner}/{repo}`                            | default-branch Code tab          |
| `GET /{owner}/{repo}/tree/{ref}/{path...}`       | `codeTree`                       |
| `GET /{owner}/{repo}/blob/{ref}/{path...}`       | `codeBlob`                       |
| `GET /{owner}/{repo}/raw/{ref}/{path...}`        | `codeRaw`                        |
| `GET /{owner}/{repo}/find/{ref}?q=...`           | `codeFinder`                     |
| `GET /{owner}/{repo}/edit/{ref}/{path...}`       | in-browser file editor           |
| `POST /{owner}/{repo}/edit/{ref}/{path...}`      | commit file edit / rename        |
| `GET /{owner}/{repo}/new/{ref}/{path...}`        | in-browser new-file editor       |
| `POST /{owner}/{repo}/new/{ref}/{path...}`       | commit new file                  |
| `GET /{owner}/{repo}/delete/{ref}/{path...}`     | delete-file confirmation         |
| `POST /{owner}/{repo}/delete/{ref}/{path...}`    | commit file deletion             |
| `GET /{owner}/{repo}/upload/{ref}/{path...}`     | upload-files form                |
| `POST /{owner}/{repo}/upload/{ref}/{path...}`    | commit uploaded files            |
| `POST /{owner}/{repo}/markdown-preview`          | editor Markdown preview fragment |
| `GET /{owner}/{repo}/actions`                    | parked product-tab shell         |
| `GET /{owner}/{repo}/projects`                   | parked product-tab shell         |
| `GET /{owner}/{repo}/wiki`                       | parked product-tab shell         |
| `GET /{owner}/{repo}/security`                   | parked product-tab shell         |
| `GET /{owner}/{repo}/pulse`                      | parked product-tab shell         |
| `GET /{owner}/{repo}/packages`                   | parked product-tab shell         |
| `GET /{owner}/{repo}/releases`                   | parked product-tab shell         |
| `GET /static/css/chroma.css`                     | runtime-generated Chroma theme   |

Every code-tab handler runs through `policy.Can(... ActionRepoRead)` —
private repos hide from anonymous viewers and unrelated users via the
existence-leak 404 guard from S15.

The in-browser mutation routes run through `policy.Can(... ActionRepoWrite)`.
They inherit the same archived-repo, suspended-user, collaborator-role,
site-admin, and private-repo existence behavior as git push surfaces.

## Repository product tabs

The repo header intentionally exposes GitHub's major product-map tabs:
Code, Issues, Pull requests, Actions, Projects, Wiki, Security and
quality, Insights, and Settings when visible to the viewer. Forks remain
available from the repo action button and About sidebar, but are not a
top-level tab on GitHub.

Actions, Projects, Wiki, Security and quality, Insights, Packages, and
Releases currently render honest parked shells via `repo/deferred_tab`.
They are public read surfaces gated by `ActionRepoRead`, so private repo
existence behavior matches Code/Issues/Pull requests while the deeper
systems remain assigned to their later sprints.

## Ref + path disambiguation

`{ref}` is the chi `*` wildcard, so the URL `/tree/feature/x/sub/file.go`
arrives as a single string. Resolution:

1. If the first segment is exactly 40 hex chars → treat as a SHA, the
   rest is the path.
2. Otherwise, longest-prefix match against the cached ref list
   (branches first, then tags, sorted longest-first). The remainder
   after the matched ref is the in-tree path.

This handles `release/v1.0/beta/CHANGELOG.md` correctly without
ambiguity. Resolution lives in `internal/repos/git/treeops.go::ResolveRef`.

When the matched ref is a raw 40-character commit SHA, the tree page
resolves the top commit summary against that SHA and displays the short
SHA in the ref switcher, matching GitHub's detached-commit tree view.

Path validation rejects `..`, control chars, leading slashes, and
backslashes — defense in depth on top of git's own validation.

## Tree listing

`git ls-tree --long --full-tree <ref>:<path>` is parsed into typed
`TreeEntry` values (`tree | blob | commit | symlink`). Sort is
directories first, then files alphabetically.

`commit` entries are git submodule pointers. When `.gitmodules` exists
on the rendered ref, the Code tab parses it once, matches entries by
submodule path, and links GitHub or configured shithub clone remotes to
the local `/{owner}/{repo}/tree/{gitlink-oid}` route when the target
repo has that commit.

If the target repo exists locally but does not have the pinned commit
object, the handler first checks `repo_source_remotes` for that target
repo. A stored source remote is the durable source of truth for imports:
the handler validates it with the shared SSRF defense, performs a
bounded, non-forced fetch of heads/tags, re-checks the object, and then
links to the exact detached-commit tree when it arrived. Successful
backfills update the target repo's default-branch OID when that ref
moved, mark the source remote fetched, and enqueue the same code-index
and size-recalc maintenance used after pushes.

GitHub URL/name inference remains as a compatibility fallback for
legacy repos that were created before source remotes existed: when the
`.gitmodules` URL is GitHub-hosted or a relative sibling that maps
cleanly to a GitHub owner/repo, shithub may fetch from the inferred
GitHub URL. Diverged local refs are never force-updated; on fetch
failure or still-missing objects, the row links to the target repo's
default Code tab so independently-created mirrors don't produce dead
links. Unknown, external, absent, or malformed remotes stay as plain
`name @ shortsha` rows.

The S17 ship excludes the htmx-driven "last commit per entry" column
that the spec describes — an extra round-trip we can add later without
a schema change. The current page renders the listing immediately.
**Deferred to S18 (commits-per-entry)** — the spec calls out this
deferral path; the tree template has the column slot ready.

## Check status indicators

The Code tab mirrors GitHub's commit-check muscle memory for commits
that have check-run data:

- the tree header shows the check rollup for the displayed head commit;
- commit list rows show the rollup beside the short SHA;
- commit detail pages show a linked check summary under the subject;
- branch and compare surfaces show matching rollups where they already
  render commit heads.

Handlers load status through `codeCommitCheckSummaries`, which batches
by `(repo_id, head_sha[])` and never queries from templates. The batch
query returns the latest check-run row per check name for each commit,
so a rerun replaces stale historical failures in the visible rollup.

Status priority is: failure/timed-out/action-required, pending,
cancelled, success, skipped, then neutral/stale. Indicators link to a
safe local `details_url` when present, otherwise to the repository
Actions list. Private repositories keep the surrounding page's
existence-leak-safe 404 behavior; no workflow names or runner details
are rendered before `repo:read` authorization succeeds.

## File view

`codeBlob` walks four cases:

* **Large** (>1 MiB): placeholder + raw download link, no body read.
* **Binary** (NUL byte in first 8 KiB): placeholder. Image extensions
  (png/jpg/jpeg/gif/webp) ≤5 MiB get an `<img>` preview pointing at
  `/raw/...`.
* **Markdown** (`.md`/`.markdown`): Goldmark + bluemonday rendered HTML
  PLUS a `<details>` source-toggle with the highlighted source.
* **Default text**: Chroma highlight by filename extension, content
  sniffing fallback.

Chroma uses the `github` style baked at process start; the CSS is
served from `/static/css/chroma.css` via a tiny in-process generator.

## In-browser file edits

The Code tab surfaces GitHub-style write affordances for users with
`repo:write` on a named branch:

- The tree header has an **Add file** dropdown with create and upload
  actions.
- Text blob headers show edit and delete icon buttons.
- The rendered README header shows an edit icon when the README was
  found in the current directory. SECURITY and CONTRIBUTING documents
  use the same blob-header controls when opened from the document tabs.

Direct web commits are intentionally limited to `refs/heads/<branch>`.
Tags and detached 40-hex commit views render read-only controls and
direct edit URLs return `400`.

`internal/repos/webedit` owns the mutation path. For each edit it:

1. Resolves the branch to its current commit and compares the submitted
   hidden `base_oid`; a mismatch returns a stale-edit conflict.
2. Builds a temporary index from the old commit with `git read-tree`.
3. Stages file changes via canonical git plumbing (`hash-object`,
   `update-index`, `write-tree`, `commit-tree`).
4. Runs `protection.Enforce` before moving the branch, so protected
   branches deny direct web commits just like pushes.
5. Advances the branch with `git update-ref <ref> <new> <old>` CAS.
6. Inserts a `push_events` row with `protocol = 'web'`, enqueues
   `push:process`, and sends the worker NOTIFY. If enqueueing fails
   after the ref has moved, the commit still succeeds and the failure
   is logged; the same post-push reconciliation gap exists for hook
   failures.

Validation rules:

- Text editor actions are capped at 1 MiB and reject NUL-byte binary
  content. Existing edit sources must be regular blobs, not symlinks,
  submodules, trees, or oversized blobs.
- Uploads are capped at 25 MiB per request and 10 MiB per file. Uploads
  may contain binary data.
- Repository paths reject empty names, leading/trailing slash, duplicate
  slash, backslash, `.`/`..` segments, control bytes, exact overwrites,
  duplicate uploads, and parent-path conflicts.
- Default commit messages are generated server-side (`Update`, `Create`,
  `Rename`, `Delete`, or `Upload`) when the form leaves the message
  blank.

The editor component is still server-rendered Go templates plus a small
page-local script. No frontend build pipeline or React/Vite layer is
required for this slice.

## Raw view

* Content-Type derived from the extension whitelist
  (`code.go::rawContentType`).
* `X-Content-Type-Options: nosniff` always.
* `Content-Security-Policy: default-src 'none'; sandbox` at the
  handler level (the global SecureHeaders middleware may overlay a
  broader CSP — both are restrictive; the OR of the two is what user
  agents enforce).
* **`Content-Disposition: attachment`** is forced for HTML, SVG, JS,
  WASM, and anything that could execute on shithub's domain. We don't
  have a separate `raw.shithub.tld` host yet (post-MVP); attachment is
  the safety belt.
* Streamed via `git cat-file -p`; never buffered. Large blobs don't
  blow up the worker's memory.

## Finder ("Go to file")

`/find/{ref}` lists every blob path on the ref via
`git ls-tree -r --name-only`, then filters with
`internal/repos/finder/finder.go::Filter`. The matcher is a
subsequence-with-bonus scorer (boundary, consecutive run, basename
hit) — not as fancy as VS Code's quickopen but good enough for tens of
thousands of paths.

Key shortcut and live-filter via htmx are spec deliverables that we
defer for now — the form-submission flow works without JS and that's
the floor S17 commits to.

## Caching

Currently **no caching layer**. Every request runs `git for-each-ref`,
`git ls-tree`, etc. That's fine for small-to-medium repos; the cost
shows up on big repos with deep trees. The S17 spec proposes a cache
keyed on `(repo_id, ref_oid, dir_path)` invalidated on push (S14's
`push:process` job is the right invalidation hook).

**Deferred** — the cache is purely performance polish. When we hit a
real-world repo where it matters, wire it in: file `internal/cache/`
plus a callback in `worker/jobs/push_process.go`. The handlers already
take a per-request `policy.Cache` so adding a per-process git cache is
mechanically straightforward.

## Pitfalls + protections

* **XSS via raw HTML/SVG**: blocked by `Content-Disposition: attachment`
  for those extensions.
* **XSS via markdown**: Goldmark configured without HTML passthrough +
  bluemonday's UGC policy on top. Tests in `internal/repos/markdown/`
  (TODO — minimal coverage today).
* **Path traversal**: `validateSubpath` in `code.go` rejects `..`,
  controls, leading slashes.
* **Web edit path traversal / overwrite**: `webedit.ValidateFilePath`
  applies the stricter mutation path guard and the service re-checks
  path existence against the commit being modified.
* **Hex collision with SHA**: ref-list lookup wins over SHA shortcut
  when the same string is both.
* **Encoding (GBK / Shift-JIS)**: TODO — text files outside UTF-8 may
  render as garbled. The body is rendered as-is; a future commit can
  add `golang.org/x/text/encoding` autodetection.

## Dependencies

* `github.com/alecthomas/chroma/v2` — syntax highlighting
* `github.com/yuin/goldmark` — CommonMark + GFM
* `github.com/microcosm-cc/bluemonday` — HTML sanitizer

## Deferred polish (tracked, not blocking)

These items are spec deliverables we ship in a later pass:

* **Last-commit-per-entry column** with htmx lazy load and pre-walked
  `git log --name-status` cache → wire into S18 (commit history) where
  the same walk powers the per-file history page.
* **Tree caching keyed on (repo_id, ref_oid, dir_path)**, push-event
  invalidation → wire into S36 (performance pass) once we have a real
  workload to measure.
* **Pagination at 1000 entries per directory** → cosmetic for huge
  trees; add when someone hits `node_modules`-grade inflation.
* **Encoding detection for non-UTF-8 source files** → file reads are
  defensive (`io.LimitReader` + size cap); render quality is the only
  loss until this lands.
