// SPDX-License-Identifier: AGPL-3.0-or-later

package highlight

import (
	"strings"
	"testing"
)

func TestRenderLinesPreservesLineCount(t *testing.T) {
	src := "package main\n\nfunc main() {\n\tprintln(\"hi\")\n}\n"
	lines := RenderLines("main.go", src)
	// strings.Split on the trailing newline produces a final empty
	// element; RenderLines mirrors that so the row table has a row
	// for the final newline (consistent with how vim / GitHub render
	// trailing-newline files).
	want := len(strings.Split(src, "\n"))
	if len(lines) != want {
		t.Fatalf("RenderLines produced %d lines; want %d", len(lines), want)
	}
}

func TestRenderLinesNoLexerFallback(t *testing.T) {
	src := "first line\nsecond line\nthird"
	lines := RenderLines("unknown.weirdext", src)
	if len(lines) != 3 {
		t.Fatalf("plain fallback produced %d lines; want 3", len(lines))
	}
	for i, want := range []string{"first line", "second line", "third"} {
		if string(lines[i]) != want {
			t.Errorf("line %d = %q; want %q", i+1, string(lines[i]), want)
		}
	}
}

func TestRenderLinesEscapesPlainText(t *testing.T) {
	lines := RenderLines("unknown.weirdext", "a < b & c > d")
	if len(lines) != 1 {
		t.Fatalf("got %d lines; want 1", len(lines))
	}
	got := string(lines[0])
	for _, want := range []string{"&lt;", "&amp;", "&gt;"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain output %q missing escape %q", got, want)
		}
	}
}

func TestRenderLinesBridgesMultiLineToken(t *testing.T) {
	// Python triple-quoted string spans 3 lines; chroma emits a single
	// <span class="s2">…</span> for the whole literal. The splitter
	// must close + reopen the span at each line boundary so each row
	// is independently well-formed.
	src := "x = '''line one\nline two\nline three'''\n"
	lines := RenderLines("a.py", src)
	if len(lines) < 3 {
		t.Fatalf("got %d lines; want at least 3", len(lines))
	}
	// Lines 1, 2, 3 each touch the multi-line string literal; each
	// should both open and close a span tag.
	for _, idx := range []int{0, 1, 2} {
		l := string(lines[idx])
		opens := strings.Count(l, "<span")
		closes := strings.Count(l, "</span>")
		if opens != closes {
			t.Errorf("line %d: %d <span vs %d </span> — not well-formed: %q",
				idx+1, opens, closes, l)
		}
	}
}

func TestSplitChromaLinesEmptyInput(t *testing.T) {
	lines := splitChromaLines("")
	if len(lines) != 1 || lines[0] != "" {
		t.Fatalf("splitChromaLines(\"\") = %v; want [\"\"]", lines)
	}
}

func TestSplitChromaLinesNoSpans(t *testing.T) {
	lines := splitChromaLines("a\nb\nc")
	if len(lines) != 3 {
		t.Fatalf("got %d lines; want 3", len(lines))
	}
	want := []string{"a", "b", "c"}
	for i, w := range want {
		if string(lines[i]) != w {
			t.Errorf("[%d] = %q; want %q", i, string(lines[i]), w)
		}
	}
}

func TestLanguageGuess(t *testing.T) {
	cases := []struct{ filename, want string }{
		{"main.go", "Go"},
		{"app.py", "Python"},
		{"index.html", "HTML"},
		{"unknown.weirdext", "Text"},
	}
	for _, tc := range cases {
		if got := LanguageGuess(tc.filename); got != tc.want {
			t.Errorf("LanguageGuess(%q) = %q; want %q", tc.filename, got, tc.want)
		}
	}
}
