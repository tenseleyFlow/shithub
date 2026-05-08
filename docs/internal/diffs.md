# Diffs

S19 ships the diff renderer wired into the S18 single-commit page.
The same renderer will host S20's compare view and S22's PR
files-changed once those sprints land.

## Pipeline

```
git diff (--patch)        →  []byte
internal/repos/diff/source     produces the raw patch bytes
                                  ▼
internal/repos/diff/parse       wraps go-gitdiff into our types
                                  ▼
internal/repos/diff/render      emits HTML for the templates
```

The shape is library-on-the-inside, package-on-the-outside: the
public surface exposes our `Diff/File/Hunk/Line` structs so we can
swap the parser later without breaking callers.

## Source helpers (`source/`)

| Function          | Git invocation                               | Use                |
| ----------------- | -------------------------------------------- | ------------------ |
| `FromCommit`      | `git diff-tree -p -r --root <sha>`           | single-commit page |
| `FromRange`       | `git diff <base>..<head>`                    | "show all changes" |
| `FromMergeBase`   | `git diff <base>...<head>` (three-dot)        | PR / compare       |

`Options` carries `IgnoreWhitespace` (`-w`) and `FindRenames` (`-M -C`).
Whitespace toggles re-source rather than parser-side filter — git's
`-w` respects language quirks (Python indentation, etc.) more
correctly than post-processing hunks.

The `--root` flag on `FromCommit` is what lets the initial parentless
commit emit its files (against the empty tree); without it the
parser sees nothing.

## Parser (`parse/`)

Wraps `github.com/bluekeyes/go-gitdiff/gitdiff`. Translates each
`*gitdiff.File` into our `File`, expanding `TextFragments` into
`Hunks` whose `Lines` carry one of four `LineKind`s:

| Kind            | Meaning                                  |
| --------------- | ---------------------------------------- |
| `LineContext`   | unchanged context line                   |
| `LineAdd`       | `+` — added line                         |
| `LineDelete`    | `-` — deleted line                       |
| `LineNoNewline` | `\ No newline at end of file` (rare)     |

Old/new line numbers are populated based on kind:
- Context: both populated
- Add: only `NewLineNo`
- Delete: only `OldLineNo`

The renderer reads `SizeBytes` (sum of line content) for the
"per-file too large" decision — cheap to compute during parse.

## Renderer (`render/`)

Two modes share the same `RenderFile` skeleton (header → hunks →
lines):

- **Unified** — old/new line numbers in two adjacent `<td>` cells,
  content in the third. The `+`/`-`/space marker is prepended to the
  content for visual consistency with `git diff`.
- **Split** — left side (old) and right side (new) in separate
  columns. Adds and deletes pair row-by-row; trailing additions on
  either side leave the other column padded.

### Per-file collapse + whole-diff truncate

| Threshold         | Default | Trigger                                                     |
| ----------------- | ------- | ----------------------------------------------------------- |
| `PerFileLineCap`  | 1000    | files with > 1000 changed lines collapse under `<details>`  |
| `PerFileBytesCap` | 500 KiB | per-file size cap                                           |
| `WholeDiffFileCap`| 100     | whole-diff truncation: every file collapses, file list eager |

The collapsed `<details>` currently renders the hunks inside the
toggle (lazy at the user's click but still inline). The spec calls
for a separate `/diff-fragment` endpoint that fetches per-file hunks
on demand; that's a polish for **S36** when we have a real big-PR
workload to bench against.

### Binary + image

Binary files emit a `shithub-diff-binary` placeholder. Image files
(by extension) render an `shithub-diff-image` placeholder note —
the side-by-side preview wires once the renderer accepts ref+path
context for `/raw/...` URLs (deferred polish; the commit page already
links the changed file in the file table).

### Highlighting

The renderer currently HTML-escapes line content; the file-level
`<a>` link to the blob view (which uses the S17 Chroma highlighter)
is the path users follow when they want syntax highlighting on a
specific change. Per-line Chroma highlighting in the diff itself is
deferred — escape is the floor; richer rendering is polish.

## Routes

The S19 changes are entirely additive to the S18 commit page. New
query parameters:

| Param         | Default | Effect                                  |
| ------------- | ------- | --------------------------------------- |
| `?diff=unified` | yes     | unified mode                            |
| `?diff=split`   |         | split mode                              |
| `?w=1`          |         | re-source with `-w` (ignore whitespace) |

The toggles emit links that preserve the other parameter so users
don't lose a mode when toggling whitespace and vice versa.

## Tests

- `parse/parse_test.go` — happy path, rename, binary, empty, multi-file.
- `render/render_test.go` — unified + split classes present, binary
  placeholder, rename header, too-large collapse, empty-diff label.

## Pitfalls handled

- **Initial-commit diff emits nothing without `--root`** — the
  source helper passes the flag.
- **`go-gitdiff` returns `(nil, _, io.EOF)` for empty input** —
  treated as "no files," no error.
- **HTML escape on every line** — content goes through
  `html.EscapeString`; only the renderer's wrapper markup is raw.
- **Whitespace toggle cache key** — currently no cache; when one
  lands in S36, the cache key must include `IgnoreWhitespace`.
- **Trailing-newline preservation** — `parse.trimNewline` strips one
  `\n`; multi-newline trailing data is preserved verbatim.

## Deferred polish (tracked)

* **`POST /{owner}/{repo}/diff-fragment?...` endpoint** — per-file
  lazy fetch for whole-diff-truncated cases. Track in S36 with the
  rest of the diff/blame caching work; current `<details>` inline
  expand is the floor.
* **Per-line Chroma highlighting in diff hunks** — every line
  currently HTML-escapes; richer rendering is post-MVP. The Chroma
  helper (`internal/repos/highlight`) is the same one the blob view
  uses, so the wiring is mechanical when desired.
* **Image diff side-by-side preview** — needs `/raw/<sha>/<path>`
  threading into the renderer. Current placeholder is honest; the
  file table already links the new path's blob.
* **`Diff` cache keyed on `(base_sha, head_sha)`** — S36, with the
  rest of the immutable-content cache layer.
