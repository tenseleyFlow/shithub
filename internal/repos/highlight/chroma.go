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
// from the same `github` style Render uses, so colors stay consistent.
func CSS() string {
	style := styles.Get("github")
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
