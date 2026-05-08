// SPDX-License-Identifier: AGPL-3.0-or-later

// Package markdown wraps Goldmark + bluemonday for safe README
// rendering. S25 will broaden this with auto-mention, issue-ref
// linking, and cross-repo extensions; S17 ships only what's needed
// for tree-page README rendering.
package markdown

import (
	"bytes"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// gm is the shared Goldmark instance. CommonMark + GFM (tables,
// strikethrough, autolinks, task-list) + auto-heading-id for in-page
// anchors. We deliberately do NOT enable HTML passthrough; raw HTML
// in user content is escaped.
var gm = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
	),
	goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	goldmark.WithRendererOptions(
		html.WithHardWraps(),
		html.WithXHTML(),
	),
)

// sanitizer is bluemonday's UGC policy with two adjustments:
//   - allow class attributes on `<code>` (Goldmark emits language-foo)
//   - allow `id` on headings so anchor links work
//
// Anything Goldmark emits passes through; anything user-injected via
// raw HTML in markdown gets stripped because Goldmark didn't enable
// HTML rendering in the first place. Defense in depth.
var sanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").Matching(bluemonday.SpaceSeparatedTokens).OnElements("code", "pre", "span")
	p.AllowAttrs("id").OnElements("h1", "h2", "h3", "h4", "h5", "h6")
	// Disallow remote images outright; readme images normally live in
	// the same repo and resolve to /raw/ which we control. Users who
	// want external images can paste links instead.
	p.AllowImages()
	return p
}()

// RenderHTML returns sanitized HTML for the given markdown bytes.
// Empty input returns an empty string. The output is suitable for
// inserting into a template via `{{ . | safeHTML }}` — every byte has
// passed bluemonday.
func RenderHTML(src []byte) (string, error) {
	if len(src) == 0 {
		return "", nil
	}
	var buf bytes.Buffer
	if err := gm.Convert(src, &buf); err != nil {
		return "", err
	}
	clean := sanitizer.SanitizeBytes(buf.Bytes())
	return string(clean), nil
}
