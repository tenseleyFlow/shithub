# Commits & blame

S18 ships the commits list, single-commit page, blame view, and the
Atom feed. Every page resolves commit author emails to shithub user
identities (display name + avatar + profile link) when there's a
verified `user_emails` row, and falls back to the raw author + a
deterministic identicon seed when there isn't.

## Routes

| Route                                                | Handler                  |
| ---------------------------------------------------- | ------------------------ |
| `GET /{owner}/{repo}/commits/{ref}/*`                | `commitsList`            |
| `GET /{owner}/{repo}/commits/{ref}.atom`             | `commitsAtom`            |
| `GET /{owner}/{repo}/commit/{sha}`                   | `commitView`             |
| `GET /{owner}/{repo}/blame/{ref}/{path...}`          | `blameView`              |

The `commits/*` and `blame/*` patterns use chi's `*` wildcard so
branches with `/` in their name (`feature/x`, `release/v1.0/beta`)
resolve correctly via `repogit.ResolveRef` (longest-prefix match
against the cached ref list, hex-SHA shortcut for 40-char first
segments). Same logic the code-tab uses.

`{sha}` accepts 7..40 hex chars; git itself disambiguates short SHAs.

## Anonymous access

Plain history, paging and per-file history (`?path=`) are public on
public repos. The author and date filters (`author`, `since`, `until`)
and the date picker's `calendar_month` are for signed-in viewers: an
anonymous request carrying any of them is redirected to
`/login?next=…` before a git process is started, and the anonymous
page renders a sign-in link in place of the two filter menus.

The reason is that the filter links are an unbounded URL space. Every
author, every day and every month on every ref (including every SHA)
is a distinct URL that forks `git log`, and the month links never run
out. On 2026-11-25 a link-following scraper spent 82% of a 9k req/min
crawl inside them (`runbooks/incidents.md#crawl-saturation`).

A well-formed SHA or ref name that is not in the repository is a 404.

## git plumbing

* `repogit.Log(ctx, gitDir, opts)` — single `git log --format=...` call
  with packed-record output (ASCII unit-separator + record-end markers
  so newlines in commit bodies don't confuse the parser). Supports
  `--max-count`, `--skip`, `--author`, `--since`, `--until`, optional
  `--follow -- <path>`.
* `repogit.GetCommit(ctx, gitDir, sha)` — `git log -1 --format=...`
  for metadata + parents + tree, then `repogit.DiffStat` for the
  per-file change rows.
* `repogit.DiffStat(ctx, gitDir, sha)` — combines
  `git diff-tree -r --root --name-status -M -C` (status + rename
  pairs) and `git diff-tree -r --root --numstat` (insert/delete
  counts, `-` flag for binary). The `--root` flag is what makes the
  initial parentless commit emit anything.
* `repogit.Blame(ctx, gitDir, opts)` — `git blame --line-porcelain`
  parsed into `BlameLine`s, then collapsed via `groupBlame` into
  `BlameChunk`s for the rendered "consecutive lines from the same
  commit collapse the gutter" UX. Caps at 5 MiB / 50k lines (returns
  `ErrBlameTooLarge`); refuses on non-blobs (`ErrBlameOnBinary`).

## Identity resolution

`internal/repos/identity/Resolver` is a per-request memo that maps
author emails to `Resolved` records:

* `User=true` → matched a verified `user_emails` row whose user is
  not suspended/deleted. Fields populated: UserID, Username,
  DisplayName, AvatarURL.
* `User=false` → unknown email; render the raw author name with a
  deterministic identicon seed (md5 hex of the lowercased email,
  matching the gravatar/identicon convention).

Construct one resolver per request and pass it through the page render.
The cache is in-process and request-scoped — across-request caching is
S36 territory.

A user with multiple verified emails (work + personal) resolves on
either: the lookup checks `user_emails` against any verified row, not
just the primary. Document this in the user-facing help when /settings
docs land.

## File-changed table on the commit view

S18 emits the rows + per-file `+X / -Y` stats; the per-file diff body
slot is left structural so S19's diff renderer drops in without
re-rendering. The status column uses git's letters: `A M D R C T`.
Renames/copies show "old → new" in the path column.

## Atom feed

Lightweight: title, id, updated, link, then per-entry id (full SHA),
title (subject), updated (author time), author name+email, summary
(commit body). The ID URN format `urn:shithub:commit:<sha>` is stable
across renames and visibility flips. The feed is capped at 50 commits.

## Issue-ref linkification (S21 hook)

Commit-body URLs are linkified inline; `#NNN` and `owner/repo#NNN`
issue refs are emitted as stable tokens
`<span data-ref="#123">#123</span>` so the S21 issue layer can
post-render-link them without re-rendering. The token shape is
documented here so the S21 enhancer doesn't have to re-derive it.

## Caching

The commit list has an ETag and an in-process page cache keyed on
`(repo_id, head_oid, page)`, invalidated on push (`caching.md`). Two
rules bound it:

- **Anonymous renders only.** The key has no viewer in it and the
  rendered page carries the viewer's nav, account switcher and CSRF
  tokens. A signed-in request neither reads nor writes the cache and
  gets no `ETag` or `Cache-Control: public`.
- **Unfiltered only.** `path`/`author`/`since`/`until` change the
  content under the same key, so filtered requests bypass it.

Blame and single-commit views run their git plumbing fresh.

## Pitfalls handled

* **Encoding in commit messages**: bodies are HTML-escaped before any
  link/ref substitution; non-UTF-8 characters render as the closest
  fallback `template.HTMLEscapeString` produces (Go's escaper accepts
  arbitrary byte slices).
* **Path traversal in blame paths**: same `validateSubpath` guard the
  code-tab uses.
* **Memory on `--line-porcelain`**: parsed in a streaming `bufio.Scanner`
  with a 1 MiB max line length; we never `io.ReadAll` the porcelain
  output.
* **Initial-commit DiffStat**: requires `--root` flag to diff against
  the empty tree. Without it, root commits show no files.
* **SHA collision with `feature/abcdef…`**: ref-list lookup wins over
  hex-SHA shortcut when the same string appears in both — same rule
  as the code-tab `ResolveRef`.
* **Blame on binary**: rejected early via `StatPath` (which already
  knows the kind from `cat-file -t`). No expensive `git blame` runs.

## Deferred polish

* **Tree last-commit-per-entry column** (`POST /tree-commits` htmx
  fragment) is the S17 deferral that lands here. The shared
  `git log --name-status` walk that powers per-file history can also
  power this column. Not wired in this commit set — the column slot
  exists in `tree.html` and the data shape is straight Log + DiffStat.
  Wire when we want the polish.
* **Caching layer** with push-event invalidation → S36.
* **Older / Newer blame navigation** (walk the chunk's commit's parent
  for that path) — UI links not yet emitted; the data path is a
  `git log -1 <chunk_sha>^ -- <path>` away. Wire when needed.
* **Signed-commit verification badge** — placeholder slot only;
  actual verification post-MVP.
