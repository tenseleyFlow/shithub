// SPDX-License-Identifier: AGPL-3.0-or-later

// Package markdown is shithub's canonical markdown rendering pipeline.
//
// One entry point: `Render(ctx, source, opts) (html, refs, mentions)`.
// Every comment, issue/PR body, README, and any future surface that
// takes user-authored markdown flows through here. No other code in
// the tree imports goldmark or bluemonday directly; a lint guard at
// `scripts/lint-markdown-boundary.sh` enforces that boundary.
//
// What's supported (CommonMark + a curated GFM set):
//
//   - Headings, paragraphs, lists, blockquotes, code blocks (fenced + indented).
//   - GFM tables, strikethrough, autolinks, task lists.
//   - `@username` mentions — resolved via Options.Resolvers.User.
//   - `#N` and `owner/repo#N` references — resolved via
//     Options.Resolvers.Issue, gated by viewer visibility.
//   - Emoji shortcodes (`:smile:`) — curated set in `emoji/`.
//   - `<details>` / `<summary>`, `<kbd>`, `<sup>`, `<sub>`,
//     task-list checkboxes (output by Goldmark with `disabled`).
//   - Code-block class allowlist: `language-*` (Chroma uses these).
//
// What's deliberately not supported:
//
//   - Raw HTML beyond the strict allowlist (we do NOT match GitHub's
//     loose HTML acceptance — we err safer).
//   - `data:` URIs (any flavor; documented in docs/markdown.md).
//   - `javascript:` / `vbscript:` URLs (rejected by sanitizer).
//   - GFM Footnotes (deferred).
//   - Math (KaTeX), Mermaid, embedded media (post-MVP).
//
// Pipeline-version contract:
//
//   - `Version` (in version.go) stamps every render.
//   - Callers store the version alongside cached HTML.
//   - On read, callers compare the stored version to `Version`; if
//     they differ, the cache is stale and the caller re-renders. We
//     never run a one-shot "re-render every comment" job — lazy.
//
// Performance budget:
//
//   - 50 KiB body, full extensions: <30 ms p99 on MVP hardware.
//   - Inputs above MaxRenderInputBytes are rejected up-front; callers
//     enforce the matching cap at the API layer.
package markdown

// MaxRenderInputBytes caps the input body. Comments / bodies are
// rejected at the API layer at 64 KiB or 256 KiB depending on
// surface; this is the renderer's defensive fallback.
const MaxRenderInputBytes = 1 << 20 // 1 MiB

// Ref is one resolved reference produced during rendering. The
// caller uses these for downstream notification fan-out (S29) and
// for the issue_references index (S21).
type Ref struct {
	// Kind is "issue" or "commit" for now.
	Kind string
	// Same-repo refs leave Owner/Repo empty.
	Owner string
	Repo  string
	// Number is set for issue refs; FullSHA for commit refs.
	Number  int64
	FullSHA string
	// Href is the resolved URL the renderer wrote into the document.
	Href string
}

// Mention is one resolved @username mention. S29's fan-out consumes
// the deduplicated list returned by Render.
type Mention struct {
	Username string
	Href     string
}
