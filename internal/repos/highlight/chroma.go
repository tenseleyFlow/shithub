// SPDX-License-Identifier: AGPL-3.0-or-later

// Package highlight wraps Chroma so the rest of the project doesn't
// import it directly. The returned HTML is Chroma's standard "html"
// formatter output with line numbers; the caller embeds it in the
// blob template inside a code-styled wrapper.
package highlight

import (
	"bytes"
	stdhtml "html"
	"path/filepath"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Render returns syntax-highlighted HTML for source. filename is used
// to guess the lexer; on miss we fall back to content sniffing, then
// finally to plain text (no highlighting). Line numbers are always on.
//
// The output is a `<pre class="chroma">…</pre>` block ready to embed
// in the page; line-number cells are linkable via Chroma's `LineLinks`
// option (rendered as `#L42`).
func Render(filename, source string) string {
	lexer := lexers.Match(filename)
	if lexer == nil {
		lexer = lexers.Analyse(source)
	}
	if lexer == nil {
		return plainPre(source)
	}
	lexer = chroma.Coalesce(lexer)
	style := styles.Get("github")
	if style == nil {
		style = styles.Fallback
	}
	formatter := chromahtml.New(
		chromahtml.WithLineNumbers(true),
		chromahtml.WithLinkableLineNumbers(true, "L"),
		chromahtml.LineNumbersInTable(true),
		chromahtml.WithClasses(true),
	)
	iter, err := lexer.Tokenise(nil, source)
	if err != nil {
		return plainPre(source)
	}
	var buf bytes.Buffer
	if err := formatter.Format(&buf, style, iter); err != nil {
		return plainPre(source)
	}
	return buf.String()
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
		chromahtml.LineNumbersInTable(true),
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

// plainPre escapes source and wraps it in a <pre> for the no-lexer
// fallback. We still provide line numbers via a <table> so the blob
// template renders consistently.
func plainPre(source string) string {
	lines := strings.Split(source, "\n")
	var lineNums, code bytes.Buffer
	for i := range lines {
		lineNums.WriteString("<a href=\"#L")
		lineNums.WriteString(itoa(i + 1))
		lineNums.WriteString("\">")
		lineNums.WriteString(itoa(i + 1))
		lineNums.WriteString("</a>\n")
	}
	for i, l := range lines {
		code.WriteString("<span id=\"L")
		code.WriteString(itoa(i + 1))
		code.WriteString("\">")
		code.WriteString(stdhtml.EscapeString(l))
		code.WriteString("</span>\n")
	}
	return `<div class="chroma"><table><tr><td class="lntable"><pre class="chroma"><code>` +
		lineNums.String() +
		`</code></pre></td><td><pre class="chroma"><code>` +
		code.String() +
		`</code></pre></td></tr></table></div>`
}

// itoa is a tiny int-to-string used inside plainPre to avoid pulling
// fmt for the hot path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
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
