// SPDX-License-Identifier: AGPL-3.0-or-later

// Package highlight wraps Chroma so the rest of the project doesn't
// import it directly. RenderLines returns one HTML fragment per
// source line — the caller composes the row + gutter table itself
// (this is the GitHub-classic / Forgejo / Gitea pattern; chroma's
// own table mode is bypassed for layout-control reasons documented
// in RenderLines).
package highlight

import (
	"bytes"
	stdhtml "html"
	"html/template"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// RenderLines tokenizes source via Chroma and returns one HTML
// fragment per line, with no surrounding `<pre>`/`<code>`/table. The
// caller composes the gutter + line table itself (S33 blob refactor).
//
// Per-line splitting respects multi-line tokens: a docstring or block
// comment that spans 5 lines yields 5 fragments, each with the open
// `<span class="…">` re-emitted at the start and a `</span>` closer
// at the end, so every fragment is independently well-formed and the
// surrounding row table can intersperse other markup safely.
//
// `filename` only drives lexer selection; the returned fragments
// don't reference it.
func RenderLines(filename, source string) []template.HTML {
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Analyse(source)
	}
	if lexer == nil {
		return plainLines(source)
	}
	lexer = chroma.Coalesce(lexer)
	style := styles.Get("github")
	if style == nil {
		style = styles.Fallback
	}
	formatter := chromahtml.New(
		chromahtml.WithClasses(true),
		chromahtml.PreventSurroundingPre(true),
	)
	iter, err := lexer.Tokenise(nil, source)
	if err != nil {
		return plainLines(source)
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iter); err != nil {
		return plainLines(source)
	}
	return splitChromaLines(buf.String())
}

// CSS returns the `<style>`-wrappable CSS for the highlight theme so
// the operator can serve it once at /static/css/chroma.css. Generated
// from BOTH the light (`github`) and dark (`github-dark`) Chroma styles
// so blob views render correctly under either theme. Each block is
// gated by `[data-theme="…"]` (the layout sets that on <html>) so only
// one set of rules is active per view. Without the dark variant the
// blob viewer renders code on a light background regardless of the
// page's theme — invisible text in dark mode.
func CSS() string {
	light := writeStyleCSS("github")
	dark := writeStyleCSS("github-dark")

	var buf bytes.Buffer
	buf.WriteString("/* light (default) — applies when [data-theme] is unset or 'light' */\n")
	buf.WriteString(prefixChromaSelectors(light, `[data-theme="light"] `, ""))
	buf.WriteString("\n/* dark */\n")
	buf.WriteString(prefixChromaSelectors(dark, `[data-theme="dark"] `, ""))
	return buf.String()
}

// writeStyleCSS emits Chroma's classes-mode CSS for a named style.
// Falls back to the Fallback style when the name is unknown.
func writeStyleCSS(name string) string {
	style := styles.Get(name)
	if style == nil {
		style = styles.Fallback
	}
	formatter := chromahtml.New(
		chromahtml.WithClasses(true),
	)
	var buf bytes.Buffer
	_ = formatter.WriteCSS(&buf, style)
	return buf.String()
}

// prefixChromaSelectors prefixes every selector in css with `prefix`
// so the rule only applies under the given theme attribute. Chroma's
// CSS rules all start with `.chroma` (or its line-number child
// classes); we walk top-level rules and prefix each.
//
// `_` is a placeholder for a future per-theme suffix (e.g. !important
// on borders) — currently unused.
func prefixChromaSelectors(css, prefix, _ string) string {
	var out bytes.Buffer
	for _, raw := range splitTopLevelRules(css) {
		rule := strings.TrimSpace(raw)
		if rule == "" {
			continue
		}
		brace := strings.IndexByte(rule, '{')
		if brace < 0 {
			out.WriteString(rule)
			continue
		}
		selectors := rule[:brace]
		body := rule[brace:]
		// Selector lists like ".chroma .nx, .chroma .nf" — prefix each.
		parts := strings.Split(selectors, ",")
		for i, p := range parts {
			parts[i] = prefix + strings.TrimSpace(p)
		}
		out.WriteString(strings.Join(parts, ", "))
		out.WriteString(" ")
		out.WriteString(body)
		out.WriteByte('\n')
	}
	return out.String()
}

// splitTopLevelRules splits a CSS blob on `}` boundaries while
// preserving the brace as part of the preceding rule. Chroma's output
// has no nested rules so naive depth-1 splitting is sufficient.
func splitTopLevelRules(css string) []string {
	var rules []string
	start := 0
	depth := 0
	for i := 0; i < len(css); i++ {
		switch css[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				rules = append(rules, css[start:i+1])
				start = i + 1
			}
		}
	}
	if start < len(css) {
		tail := strings.TrimSpace(css[start:])
		if tail != "" {
			rules = append(rules, tail)
		}
	}
	return rules
}

// plainLines is the no-lexer fallback: HTML-escape each line and
// hand it back. No syntax highlighting; the row table handles the
// gutter + line layout the same way it does for a chroma'd file.
func plainLines(source string) []template.HTML {
	if source == "" {
		// A truly empty file still gets one row so the panel chrome
		// renders consistently. The line is the empty string.
		return []template.HTML{template.HTML("")}
	}
	raw := strings.Split(source, "\n")
	out := make([]template.HTML, len(raw))
	for i, l := range raw {
		out[i] = template.HTML(stdhtml.EscapeString(l)) //nolint:gosec // EscapeString output is safe HTML
	}
	return out
}

// splitChromaLines walks chroma's classes-mode HTML and returns one
// fragment per source line. The wrinkle: chroma may wrap a multi-line
// token (docstring, block comment, raw string literal) in a single
// `<span class="…">…</span>` that crosses line boundaries. A naive
// strings.Split on '\n' would leave half-open spans in some lines and
// orphan `</span>` in others, breaking the row table.
//
// The walker tracks the open-span stack: at every '\n' it closes any
// currently-open spans, emits the line, then reopens the same spans
// at the start of the next line. The result: each line's HTML is
// independently well-formed, and a multi-line token still carries
// the same CSS class on every line it touches.
func splitChromaLines(html string) []template.HTML {
	var (
		lines    []template.HTML
		openTags []string // verbatim "<span …>" strings, used to reopen
		cur      strings.Builder
	)
	closeAll := func() {
		for range openTags {
			cur.WriteString("</span>")
		}
	}
	reopenAll := func() {
		for _, t := range openTags {
			cur.WriteString(t)
		}
	}

	i := 0
	for i < len(html) {
		switch {
		case strings.HasPrefix(html[i:], "<span"):
			end := strings.IndexByte(html[i:], '>')
			if end < 0 {
				// Malformed; bail to a single-line emit so the caller
				// at least gets unbroken markup.
				cur.WriteString(html[i:])
				i = len(html)
				continue
			}
			tag := html[i : i+end+1]
			cur.WriteString(tag)
			openTags = append(openTags, tag)
			i += end + 1
		case strings.HasPrefix(html[i:], "</span>"):
			cur.WriteString("</span>")
			if len(openTags) > 0 {
				openTags = openTags[:len(openTags)-1]
			}
			i += len("</span>")
		case html[i] == '\n':
			closeAll()
			lines = append(lines, template.HTML(cur.String())) //nolint:gosec // assembled from chroma + escaped tokens
			cur.Reset()
			reopenAll()
			i++
		default:
			cur.WriteByte(html[i])
			i++
		}
	}
	// Trailing line (no terminating \n).
	closeAll()
	if cur.Len() > 0 || len(lines) == 0 {
		lines = append(lines, template.HTML(cur.String())) //nolint:gosec // see above
	}
	return lines
}

// LanguageGuess returns the human-readable language name (or "Text"
// fallback) for display in the blob viewer's header.
func LanguageGuess(filename string) string {
	if lexer := lexers.Match(filename); lexer != nil {
		return lexer.Config().Name
	}
	if ext := filepath.Ext(filename); ext != "" {
		if l := lexers.Get(strings.TrimPrefix(ext, ".")); l != nil {
			return l.Config().Name
		}
	}
	return "Text"
}
