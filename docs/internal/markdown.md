# Markdown pipeline (internal)

S25 ships shithub's canonical markdown renderer. One package owns
goldmark + bluemonday; every other package routes through
`markdown.Render`. The boundary is enforced by
`scripts/lint-markdown-boundary.sh`.

## Architecture

```
internal/markdown/
    markdown.go          — package doc + Ref/Mention public types
    version.go           — Version int32 = 1 (pipeline stamp)
    opts.go              — Options + Resolvers structs
    render.go            — Render() entry point + RenderHTML shim
    sanitize.go          — bluemonday policy
    markdown_test.go     — XSS fixture suite + golden render tests
    extensions/
        extensions.go    — single ASTTransformer for refs/mentions/commits/emoji
        emoji.go         — curated shortcode → unicode map
```

## Render pipeline

```
source bytes
    │
    ▼
[goldmark.Convert]  CommonMark + GFM (tables, strikethrough,
    │                autolinks, task lists), html.WithUnsafe
    │                so raw HTML reaches the sanitizer
    │
    ▼
[ASTTransformer]    walks Document, skips code/codespan/link/
    │                image/HTML subtrees, runs reCombined regex
    │                on each Text segment, replaces matches with
    │                Link nodes (mentions/refs/commits) or String
    │                nodes (emoji + plain-text fallbacks).
    │
    ▼
[bluemonday.SanitizeBytes] strict UGC policy: scheme allowlist
    │                       (http/https/mailto), <details>/<summary>/
    │                       <kbd>/<sup>/<sub>, language-* class on
    │                       code, no <script>/<style>/<iframe>/data:
    │
    ▼
clean HTML bytes
```

Each Render call builds a fresh `goldmark.Markdown` so the
extension's per-call Options can plug in without races. The build
is ~µs and removes any cross-render contamination of the
transformer state.

## Reference resolution

`Options.Resolvers` is the seam between the renderer and the rest
of the runtime. Each resolver is optional; a nil resolver means
"render that flavor as plain text" — no link, no error, no
existence leak.

Visibility gating happens *inside* the resolver. The transformer
trusts the resolver's `ok` return — if the resolver returns
`ok=false`, the displayed text passes through as plain text.

The handler/orchestrator that wires the resolver MUST honor:

- `User`: rejects suspended users; respects org-team visibility
  once S30/S31 land.
- `Issue`: rejects when the repo isn't visible to the viewer (the
  S15 `policy.Can(ActionIssueRead)` gate).
- `Commit`: rejects when the SHA prefix doesn't match a real
  commit; doesn't care about visibility (commit URLs respect repo
  visibility at request time).

## Sanitizer policy decisions

The base is `bluemonday.UGCPolicy()`. We diverge by:

- Allowing `<details>`, `<summary>`, `<kbd>`, `<sup>`, `<sub>`.
  Common in READMEs; not security-relevant.
- Allowing `id` on h1-h6 so anchor links work
  (Goldmark's `WithAutoHeadingID`).
- Allowing `class` on `<code>`, `<pre>`, `<span>` matching
  `^(?:language-[A-Za-z0-9_+-]+|chroma|chroma-[a-zA-Z]+|nl|ln|line|hl)$`
  so Chroma's syntax-highlighted output passes through.
- Allowing `<input type="checkbox" disabled checked>` — Goldmark's
  task-list output. The `disabled` and `checked` attributes are
  HTML boolean attrs; we don't constrain their values.
- Restricting URL schemes to `http`, `https`, `mailto` (no `data:`,
  no `javascript:`, no `vbscript:`, no `ftp:`). This is stricter
  than UGCPolicy's default and is the documented divergence from
  GitHub.

## Pipeline-version stamp

`Version` (in `version.go`) is bumped when:

- The sanitizer policy changes (added/removed tag, attribute,
  scheme).
- A new AST extension or rendering output change.
- Goldmark / bluemonday major-version upgrade with output drift.

Callers store the rendered version alongside `body_html_cached`
columns. On read, callers compare the stored version to
`Version`; if they differ, the cache is stale and the caller
re-renders. We never run a one-shot "re-render every comment" job.

## Why ASTTransformer instead of inline parsers

Goldmark inline parsers run during the main parse pass and need a
`Trigger() []byte` plus careful interaction with Goldmark's
existing inline disambiguation (link/codespan/emphasis priorities).

A single `parser.ASTTransformer` walks the document after parsing,
visits text nodes whose ancestors aren't code/codespan/link/image/
HTML, and runs one combined regex per node. The replacement
inserts `*ast.Link` (mentions/refs/commits) or `*ast.String`
(emoji + plain fallbacks) before the original text node, then
removes the original.

Tradeoffs:

- ✅ Simpler than custom inline parsers.
- ✅ Trivial to add new patterns (extend `reCombined`).
- ✅ Single-pass — runs once per text node.
- ❌ Can't span across other inline nodes (e.g. a mention split
  by `*emphasis*`). That's fine — we don't want that anyway.

## Hostile-input fixture suite

`TestRender_HostileInputs` is the test of record. Every fixture
is an XSS vector; the pass condition is "rendered HTML contains
no executable JS surface" — no `<script>`, no `href="javascript:"`,
no `on*` handlers, no `<iframe>` / `<object>` / `<embed>` /
`<base>` / `<meta>` / `<style>`, no `expression()`, no
`<annotation-xml>`.

Adding a vector when a CVE / advisory lands is cheap; the test
file lists ~40 vectors today.

## Performance budget

- 50 KiB body, full extensions: <30 ms p99 on MVP hardware.
- Inputs above `MaxRenderInputBytes` (1 MiB) are rejected by the
  defensive check in `Render`. The API layer enforces tighter
  per-surface caps (64 KiB or 256 KiB depending on context); this
  is the renderer's last-resort guardrail.

## Refactor pass (S25)

S17/S18/S21/S22/S23/S24 used a per-sprint
`internal/repos/markdown.RenderHTML` wrapper. S25 deleted that
package and updated every importer to
`github.com/tenseleyFlow/shithub/internal/markdown`. The shim
`markdown.RenderHTML(src) (string, error)` is preserved so the
swap is a one-line import edit per file.

Callers that want richer behavior — resolved refs, mentions,
viewer-aware visibility — should call `markdown.Render` directly.
The interim `RenderHTML` keeps a sensible default (SoftBreakAsBR
on, no resolvers).

## README presentation chrome

Repository READMEs use the same sanitized HTML pipeline as comments and
issues. The repo page adds GitHub-parity presentation chrome in the web
layer only:

- `<pre>` blocks inside `.markdown-body` are wrapped client-side with a
  stable code-block container and a copy button. The copied text comes
  from the original `<code>` or `<pre>` textContent, not from injected
  controls.
- The README outline menu is populated client-side from rendered `h1`-
  `h6` elements. Goldmark-generated IDs are reused. Raw HTML headings
  without IDs get deterministic page-local slugs so the outline can link
  to them.
- The JavaScript only reads sanitized text/IDs and mutates local chrome;
  it is not part of the trust boundary. Sanitization remains entirely in
  `internal/markdown`.

## Lint guard

`scripts/lint-markdown-boundary.sh` fails CI when goldmark or
bluemonday is imported outside `internal/markdown/`. Test files
are exempt. Anything else triggers the alarm; the fix is to
swap the import to the canonical package.

Wired into `make ci` via the `lint-markdown` target.

## Deferred

| Feature                  | Destination |
| ------------------------ | ----------- |
| KaTeX math rendering     | post-MVP    |
| Mermaid diagrams         | post-MVP    |
| GFM Footnotes            | post-MVP    |
| Single-flight cache wrap | S36 (perf pass) — only if cache-stampede on version bump bites |
| Per-line Chroma in diff hunks | S36 (already deferred)  |
| `data:image/...` allowlist (sized) | post-MVP — if README writers complain enough |
| `@org/team` mentions     | S31 (teams)  |
