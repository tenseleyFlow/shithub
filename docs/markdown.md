# Markdown on shithub

shithub renders user-authored markdown — issue bodies, PR
descriptions, comments, READMEs — through one canonical pipeline.
This page documents what's supported and what's deliberately not.

## Supported

### CommonMark + GFM

The full CommonMark spec plus the curated GFM additions:

- Headings (`# Title` through `###### h6`) with auto-generated
  anchor IDs (`<h1 id="title">`).
- Paragraphs, soft line breaks (rendered as `<br>` in
  comment-style contexts; preserved as whitespace in READMEs).
- Bullet, numbered, and **task lists** (`- [x]` / `- [ ]`).
- Block quotes, fenced + indented code blocks.
- Inline `code`, **bold**, *italic*, ~~strikethrough~~.
- Tables (GFM pipe syntax).
- Autolinks (`https://example.com` becomes a link automatically).

### Code blocks with syntax highlighting

Fenced code with a language tag turns on Chroma highlighting:

````
```go
fmt.Println("hello")
```
````

Languages we recognize: every language Chroma supports (~250).
Unknown languages render as plain `<pre><code>` with no
highlighting.

### shithub-specific inline patterns

| You write             | We render                                            |
| --------------------- | ---------------------------------------------------- |
| `@alice`              | Link to `/alice` if the user exists                  |
| `#42`                 | Link to issue/PR #42 in the current repo, if visible |
| `alice/proj#42`       | Cross-repo issue/PR link, if visible to you          |
| `abc1234`             | Commit link in the current repo (7+ hex chars)       |
| `:rocket:` / `:+1:`   | Emoji from a curated set (~150 shortcodes)          |

These patterns *do not match inside code blocks or inline code* —
`` `#42` `` stays literal.

If a reference can't be resolved (the issue doesn't exist, the
user doesn't exist, the cross-repo target isn't visible to you),
we render the text as-is. No broken links, no "deleted" labels,
no existence leaks.

### Safe HTML (allowlisted)

These tags pass through unchanged:

- `<details>` / `<summary>` (collapsible sections)
- `<kbd>` (keyboard markers)
- `<sup>`, `<sub>` (superscript / subscript)
- Standard text formatting tags Goldmark emits (em, strong, code,
  pre, blockquote, ul, ol, li, table family).

## Not supported

We deliberately do **not** match GitHub's looser markdown surface:

| Feature                  | Why                                                 |
| ------------------------ | --------------------------------------------------- |
| Raw HTML beyond allowlist | XSS prevention. Anything outside the list is stripped. |
| `data:` URIs              | Avoids tracking pixels and decompression bombs.      |
| `javascript:` URLs        | Always XSS.                                          |
| `<script>`, `<style>`, `<iframe>`, `<object>`, `<embed>`, `<base>`, `<meta>` | XSS / unwanted side effects. |
| Inline event handlers (`onclick`, `onerror`, etc.) | XSS.                  |
| Math (KaTeX)              | Post-MVP.                                           |
| Mermaid diagrams          | Post-MVP.                                           |
| GFM Footnotes             | Deferred — file an issue if you want them.          |

For inline images, repo-relative paths work via the `/raw/` route:
`![diagram](docs/img/diagram.png)`. External-host images are also
allowed; remote tracking pixels are inherent to that — we don't
proxy.

## Newline handling

There are two render modes, picked per surface:

- **Comment / issue / PR body**: newlines render as `<br>`
  (matches GitHub's UI). You can write a paragraph by leaving a
  blank line.
- **README and other structured docs**: standard CommonMark
  newline rules (paragraphs separated by blank lines, soft
  newlines join words).

## Cache + version

Rendered HTML is cached on the source row alongside a pipeline
version. Bumping the renderer (a sanitizer-policy change, a new
extension, a major Goldmark/bluemonday upgrade with output drift)
re-renders comments lazily on next read — we never run a "re-render
every comment" batch.

## Contributing

Markdown changes go through `internal/markdown/`. The boundary is
enforced: importing `goldmark` or `bluemonday` outside that
package fails CI (`scripts/lint-markdown-boundary.sh`).

If a new XSS vector lands in the wild, add a fixture to
`internal/markdown/markdown_test.go::TestRender_HostileInputs` and
fix the policy.
