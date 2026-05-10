// SPDX-License-Identifier: AGPL-3.0-or-later

package markdown

import (
	"context"
	"strings"
	"testing"
)

// TestRender_HostileInputs is the XSS-vector cheatsheet. Every
// fixture is a markdown body that *attempts* to inject executable
// JS through a different vector. The pass condition: the rendered
// HTML contains no `<script` tag, no `javascript:` URL, no event
// handler attribute (`on*`), and no `data:` URI.
//
// Add new vectors here when a CVE / advisory lands in goldmark or
// bluemonday — they're cheap to keep.
func TestRender_HostileInputs(t *testing.T) {
	t.Parallel()
	vectors := []string{
		// Direct script tag.
		`<script>alert(1)</script>`,
		`<SCRIPT>alert(1)</SCRIPT>`,
		`<script src="//evil.com/x.js"></script>`,
		// Inline event handlers.
		`<img src="x" onerror="alert(1)">`,
		`<img src=x onerror=alert(1)>`,
		`<a onmouseover="alert(1)">x</a>`,
		`<body onload="alert(1)">`,
		// Style with expressions.
		`<style>body{background:url("javascript:alert(1)")}</style>`,
		`<div style="background:url(javascript:alert(1))">x</div>`,
		// javascript: links.
		`[click](javascript:alert(1))`,
		`<a href="javascript:alert(1)">x</a>`,
		`<a href="JaVaScRiPt:alert(1)">x</a>`,
		`[click](JAVASCRIPT:alert(1))`,
		// data: URIs (we disallow even data:image).
		`<img src="data:image/svg+xml;base64,PHN2Zz4=">`,
		`[x](data:text/html,<script>alert(1)</script>)`,
		// vbscript:.
		`<a href="vbscript:msgbox(1)">x</a>`,
		// SVG-embedded scripts.
		`<svg><script>alert(1)</script></svg>`,
		`<svg onload="alert(1)"></svg>`,
		// iframes.
		`<iframe src="//evil.com"></iframe>`,
		`<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
		// HTML in markdown link text doesn't escape sanitizer.
		`[<script>alert(1)</script>](https://example.com)`,
		// Mutation XSS via mismatched quotes.
		`<a href="x"onmouseover="alert(1)">x</a>`,
		// Encoded payloads.
		`<a href="&#x6A;avascript:alert(1)">x</a>`,
		`<a href="&#106;avascript:alert(1)">x</a>`,
		// Backticked code-like content shouldn't escape.
		"`<script>alert(1)</script>`",
		// Embedded in autolinks.
		`<javascript:alert(1)>`,
		// Object/embed.
		`<object data="x.swf"></object>`,
		`<embed src="x.swf">`,
		// Form/button with formaction.
		`<form><button formaction="javascript:alert(1)">x</button></form>`,
		// Meta refresh.
		`<meta http-equiv="refresh" content="0; url=javascript:alert(1)">`,
		// Base href hijack.
		`<base href="javascript:">`,
		// MathML / annotation.
		`<math><annotation-xml encoding="text/html"><script>alert(1)</script></annotation-xml></math>`,
		// CSS expression (legacy IE).
		`<div style="width: expression(alert(1))">x</div>`,
		// Nested fenced code with a script.
		"```\n<script>alert(1)</script>\n```",
		// Markdown link href with newlines.
		"[x](\njavascript:alert(1))",
		// Image with javascript:.
		`![x](javascript:alert(1))`,
		// HTML entities in URI.
		`[x](java&#0000115;cript:alert(1))`,
		// Hex / decimal entities in href attribute.
		`<a href="javasc&#x72;ipt:alert(1)">x</a>`,
		// Tab/newline obfuscation.
		"<a href=\"java\tscript:alert(1)\">x</a>",
		"<a href=\"java\nscript:alert(1)\">x</a>",
		// Polyglot HTML+SVG.
		`<svg/onload=alert(1)>`,
		// Anchor with target=_blank but no rel (we want rel auto-set).
		`<a href="https://evil.com" target="_blank">x</a>`,
	}
	for i, src := range vectors {
		out, _, _, err := Render(context.Background(), []byte(src), Options{})
		if err != nil {
			t.Fatalf("vector %d render error: %v", i, err)
		}
		// Lower-case for case-insensitive substring search. We
		// distinguish "executable surface" from "harmless text".
		// Plain-text "javascript:" in prose is safe; "javascript:"
		// inside href/src is an XSS — guard the latter shape only.
		s := strings.ToLower(string(out))
		for _, bad := range []string{
			"<script", "</script>",
			`href="javascript:`, `href='javascript:`,
			`src="javascript:`, `src='javascript:`,
			`href="vbscript:`, `src="vbscript:`,
			`href="data:`, `src="data:text`, `src="data:image`,
			" onerror=", " onload=", " onclick=", " onmouseover=",
			"<iframe", "<object", "<embed",
			"<style", "<base ", "<meta ",
			"<annotation-xml", "expression(",
		} {
			if strings.Contains(s, bad) {
				t.Errorf("vector %d (%q): rendered HTML contains %q\nout=%q", i, src, bad, out)
			}
		}
	}
}

// TestRender_AllowsSafeHTML ensures the strict policy doesn't strip
// `<details>`, `<summary>`, `<kbd>`, `<sup>`, `<sub>`, task-list
// checkboxes, language-* class on code blocks, or auto-heading IDs.
func TestRender_AllowsSafeHTML(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		src         string
		mustContain []string
	}{
		{
			"details + summary",
			"<details><summary>click</summary>secret</details>",
			[]string{"<details>", "<summary>", "click", "secret"},
		},
		{
			"kbd",
			"press <kbd>Ctrl</kbd>+<kbd>C</kbd>",
			[]string{"<kbd>Ctrl</kbd>", "<kbd>C</kbd>"},
		},
		{
			"sup/sub",
			"x<sup>2</sup> + y<sub>i</sub>",
			[]string{"<sup>2</sup>", "<sub>i</sub>"},
		},
		{
			"task list",
			"- [x] done\n- [ ] not yet\n",
			[]string{"<input", "checkbox", "disabled"},
		},
		{
			"fenced code with language",
			"```go\nfmt.Println(\"hi\")\n```",
			[]string{`class="language-go"`},
		},
		{
			"heading anchor id",
			"# Hello world",
			[]string{`id="hello-world"`},
		},
		{
			"readme presentation html",
			`<p align="center"><img src="logo.svg" alt="" width="120"></p><h1 align="center">shithub</h1>`,
			[]string{`<p align="center">`, `<img`, `src="logo.svg"`, `width="120"`, `<h1 align="center">`},
		},
		{
			"GFM table",
			"| a | b |\n|---|---|\n| 1 | 2 |\n",
			[]string{"<table>", "<th>a</th>", "<td>1</td>"},
		},
		{
			"strikethrough",
			"~~obsolete~~",
			[]string{"<del>obsolete</del>"},
		},
		{
			"autolink",
			"https://example.com",
			[]string{`href="https://example.com"`},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, _, _, err := Render(context.Background(), []byte(c.src), Options{})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			s := string(out)
			for _, want := range c.mustContain {
				if !strings.Contains(s, want) {
					t.Errorf("expected %q in output, got %q", want, s)
				}
			}
		})
	}
}

// TestRender_MentionResolution checks that @user resolves when the
// resolver returns ok and stays plain text otherwise.
func TestRender_MentionResolution(t *testing.T) {
	t.Parallel()
	resolver := func(_ context.Context, name string) (string, bool) {
		if name == "alice" {
			return "/alice", true
		}
		return "", false
	}
	out, _, mentions, err := Render(context.Background(), []byte("hi @alice and @bob"), Options{
		Resolvers: Resolvers{User: resolver},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `href="/alice"`) {
		t.Errorf("expected @alice link, got %q", s)
	}
	if strings.Contains(s, `href="/bob"`) {
		t.Errorf("@bob should not link, got %q", s)
	}
	if len(mentions) != 1 || mentions[0].Username != "alice" {
		t.Errorf("expected 1 mention (alice), got %v", mentions)
	}
}

// TestRender_TeamMentionResolution: @org/team renders via the Team
// resolver and falls back to plain text when the resolver declines
// (e.g. secret team invisible to viewer). S31.
func TestRender_TeamMentionResolution(t *testing.T) {
	t.Parallel()
	teamResolver := func(_ context.Context, org, team string, _ int64) (string, bool) {
		if org == "acme" && team == "eng" {
			return "/acme/teams/eng", true
		}
		return "", false
	}
	out, _, _, err := Render(context.Background(), []byte("ping @acme/eng and @acme/secret here"), Options{
		Resolvers: Resolvers{Team: teamResolver},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `href="/acme/teams/eng"`) {
		t.Errorf("expected @acme/eng link, got %q", s)
	}
	if strings.Contains(s, `href="/acme/teams/secret"`) {
		t.Errorf("@acme/secret should not link, got %q", s)
	}
}

// TestRender_IssueRefResolution checks both same-repo and cross-repo
// refs, and that an unresolvable ref renders as plain text (no link).
func TestRender_IssueRefResolution(t *testing.T) {
	t.Parallel()
	resolver := func(_ context.Context, owner, name string, num int64, _ int64) (string, bool) {
		// Same-repo refs leave owner+name empty.
		if owner == "" && name == "" && num == 7 {
			return "/o/r/issues/7", true
		}
		if owner == "alice" && name == "proj" && num == 3 {
			return "/alice/proj/issues/3", true
		}
		return "", false
	}
	out, refs, _, err := Render(context.Background(), []byte("see #7 and alice/proj#3, but not bob/x#9"), Options{
		Resolvers: Resolvers{Issue: resolver},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `href="/o/r/issues/7"`) {
		t.Errorf("expected #7 link, got %q", s)
	}
	if !strings.Contains(s, `href="/alice/proj/issues/3"`) {
		t.Errorf("expected alice/proj#3 link, got %q", s)
	}
	if strings.Contains(s, `href="/bob/x/issues/9"`) {
		t.Errorf("bob/x#9 should not link, got %q", s)
	}
	if len(refs) != 2 {
		t.Errorf("expected 2 refs, got %v", refs)
	}
}

// TestRender_RefsInsideCodeAreInert confirms that #N inside inline
// code or fenced code stays as text.
func TestRender_RefsInsideCodeAreInert(t *testing.T) {
	t.Parallel()
	resolver := func(_ context.Context, owner, name string, num int64, _ int64) (string, bool) {
		return "/should/not/appear", true
	}
	src := "Inline `#7` and:\n\n```\nblock #7 here\n```"
	out, refs, _, err := Render(context.Background(), []byte(src), Options{
		Resolvers: Resolvers{Issue: resolver},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(out), "/should/not/appear") {
		t.Errorf("ref leaked into code block: %q", out)
	}
	if len(refs) != 0 {
		t.Errorf("expected 0 refs inside code, got %v", refs)
	}
}

// TestRender_EmojiShortcodes checks the curated set works.
func TestRender_EmojiShortcodes(t *testing.T) {
	t.Parallel()
	out, _, _, err := Render(context.Background(), []byte("ship it :rocket: :+1: :notrealemoji:"), Options{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "🚀") {
		t.Errorf("expected rocket emoji in output, got %q", s)
	}
	if !strings.Contains(s, "👍") {
		t.Errorf("expected +1 emoji in output, got %q", s)
	}
	if !strings.Contains(s, ":notrealemoji:") {
		t.Errorf("unknown shortcode should pass through, got %q", s)
	}
}

// TestRender_InputTooLarge enforces the renderer's defensive cap.
func TestRender_InputTooLarge(t *testing.T) {
	t.Parallel()
	big := make([]byte, MaxRenderInputBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if _, _, _, err := Render(context.Background(), big, Options{}); err == nil {
		t.Errorf("expected ErrInputTooLarge")
	}
}

// TestRender_SoftBreakAsBR controls the comment-vs-readme newline
// handling.
func TestRender_SoftBreakAsBR(t *testing.T) {
	t.Parallel()
	src := "line one\nline two\n"
	br, _, _, _ := Render(context.Background(), []byte(src), Options{SoftBreakAsBR: true})
	noBR, _, _, _ := Render(context.Background(), []byte(src), Options{SoftBreakAsBR: false})
	if !strings.Contains(string(br), "<br") {
		t.Errorf("SoftBreakAsBR=true: expected <br>, got %q", br)
	}
	if strings.Contains(string(noBR), "<br") {
		t.Errorf("SoftBreakAsBR=false: should not contain <br>, got %q", noBR)
	}
}

// TestRender_BackCompatRenderHTML keeps the old shim working so the
// interim S17/S21/S22 callers don't need rewrite during S25.
func TestRender_BackCompatRenderHTML(t *testing.T) {
	t.Parallel()
	html, err := RenderHTML([]byte("**bold** text"))
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(html, "<strong>bold</strong>") {
		t.Errorf("expected bold, got %q", html)
	}
}
