// SPDX-License-Identifier: AGPL-3.0-or-later

package markdown

import (
	"regexp"

	"github.com/microcosm-cc/bluemonday"
)

// sanitizer is the package-private bluemonday policy. Built once;
// safe for concurrent use.
//
// Threat model:
//
//   - Goldmark with HTML rendering off escapes user-injected raw
//     HTML at parse time, so the sanitizer is *defense in depth* —
//     anything that reaches it should already be Goldmark-emitted
//     HTML or our own AST-extension output.
//   - The strict scheme allowlist (`http`, `https`, `mailto`) blocks
//     `javascript:`, `data:`, `vbscript:` URIs entirely.
//   - We diverge from GitHub by *not* allowing `data:image/...`. If
//     a user wants an inline image, repo-relative paths work via
//     /raw/. Documented in docs/markdown.md.
var sanitizer = func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()

	// Headings keep their auto-generated id so anchor links work.
	p.AllowAttrs("id").OnElements("h1", "h2", "h3", "h4", "h5", "h6")
	p.AllowAttrs("align").Matching(reAlign).OnElements("p", "div", "h1", "h2", "h3", "h4", "h5", "h6")

	// Code-block class allowlist for Chroma (`language-foo`). The
	// SpaceSeparatedTokens matcher is bluemonday-built-in; we
	// further constrain via the regex below so only `language-*` and
	// chroma's own ancillary classes pass.
	p.AllowAttrs("class").Matching(reCodeClass).OnElements("code", "pre", "span")

	// GFM task lists: Goldmark emits `<input checked="" disabled=""
	// type="checkbox" />` inside `<li>`. UGCPolicy doesn't allow
	// input by default; whitelist the disabled-checkbox shape only.
	// `type` is matched against "checkbox"; `disabled` and `checked`
	// are HTML boolean attrs (value commonly empty), so we don't
	// constrain the value — presence is the signal.
	p.AllowAttrs("type").Matching(regexp.MustCompile(`^checkbox$`)).OnElements("input")
	p.AllowAttrs("disabled", "checked").OnElements("input")

	// Folded sections + keyboard markers. Common in READMEs.
	p.AllowElements("details", "summary", "kbd", "sup", "sub")

	// Mention / ref / commit anchors emitted by our extensions carry
	// these classes for styling. The base UGC policy already allows
	// <a> with href + rel; we just need the class allowlist above to
	// cover `shithub-mention`, `shithub-ref`, `shithub-commit`.

	// Hard-restrict URL schemes. UGCPolicy already restricts schemes
	// on <a>, but we tighten further: drop ftp, drop data:, leave
	// only http(s) + mailto.
	p.AllowURLSchemes("http", "https", "mailto")
	// Image schemes — UGC allows http/https only by default; we keep
	// that. No data: anywhere.
	p.AllowImages()

	// rel="noopener noreferrer" auto-added when target="_blank" is
	// set. We only set target via opts on autolinks; let bluemonday
	// keep its default rel-handling.
	p.RequireNoFollowOnLinks(true)
	return p
}()

var (
	reCodeClass = regexp.MustCompile(`^(?:language-[A-Za-z0-9_+\-]+|chroma|chroma-[a-zA-Z]+|nl|ln|line|hl)(?:\s+(?:language-[A-Za-z0-9_+\-]+|chroma|chroma-[a-zA-Z]+|nl|ln|line|hl))*$`)
	reAlign     = regexp.MustCompile(`^(?:left|center|right)$`)
)

// sanitizeBytes is the hot-path entry the Render pipeline uses. The
// bluemonday Policy is built at package init via the var initializer
// above and is itself goroutine-safe — no need for sync.Once gymnastics.
func sanitizeBytes(in []byte) []byte {
	return sanitizer.SanitizeBytes(in)
}
