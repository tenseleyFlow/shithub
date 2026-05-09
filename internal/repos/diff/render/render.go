// SPDX-License-Identifier: AGPL-3.0-or-later

// Package render produces HTML for parsed diffs. Two modes share the
// same per-file structure (header + hunks); only line-level layout
// differs. Per-line syntax highlighting via Chroma; binary and image
// files get their own placeholders.
//
// The renderer doesn't know about HTTP — callers wrap the output in
// `template.HTML` for the response. This keeps it usable from the
// commit page (S18), compare page (S20), and PR files-changed page
// (S22) without leaking response shape into the rendering surface.
package render

import (
	"bytes"
	"fmt"
	"html"
	"path"
	"strings"

	"github.com/tenseleyFlow/shithub/internal/repos/diff/parse"
	"github.com/tenseleyFlow/shithub/internal/repos/highlight"
)

// Mode picks unified or split rendering.
type Mode string

const (
	ModeUnified Mode = "unified"
	ModeSplit   Mode = "split"
)

// Options tunes a render. HighlightCap caps per-file highlight cost;
// past it the renderer falls back to plain `<pre>`. Zero uses the
// 200 KiB default from the spec.
type Options struct {
	Mode             Mode
	HighlightCap     int
	PerFileLineCap   int // collapse files with > this many changed lines (0 = 1000 default)
	PerFileBytesCap  int // collapse files larger than this (0 = 500 KiB)
	WholeDiffFileCap int // truncate the whole-diff after this many files (0 = 100)
}

// Default returns the spec defaults so callers don't recompute them.
func defaults(o Options) Options {
	if o.Mode == "" {
		o.Mode = ModeUnified
	}
	if o.HighlightCap == 0 {
		o.HighlightCap = 200 * 1024
	}
	if o.PerFileLineCap == 0 {
		o.PerFileLineCap = 1000
	}
	if o.PerFileBytesCap == 0 {
		o.PerFileBytesCap = 500 * 1024
	}
	if o.WholeDiffFileCap == 0 {
		o.WholeDiffFileCap = 100
	}
	return o
}

// Diff renders a full Diff to HTML. Entry point for the templates.
// The output is one big string of HTML; for very large diffs callers
// can hit RenderFile instead and stream per-file.
func Diff(d *parse.Diff, opts Options) string {
	opts = defaults(opts)
	if d == nil || len(d.Files) == 0 {
		return `<div class="shithub-diff-empty">No changes.</div>`
	}
	var buf bytes.Buffer
	totalFiles := len(d.Files)
	collapseAll := totalFiles > opts.WholeDiffFileCap
	for i := range d.Files {
		f := &d.Files[i]
		// Per-file collapse decision.
		lineCount := 0
		for _, h := range f.Hunks {
			lineCount += len(h.Lines)
		}
		f.IsTooLarge = collapseAll ||
			lineCount > opts.PerFileLineCap ||
			f.SizeBytes > opts.PerFileBytesCap
		buf.WriteString(RenderFile(f, opts))
	}
	if collapseAll {
		fmt.Fprintf(&buf,
			`<div class="shithub-diff-truncated">Diff truncated: %d files; expand each to load its hunks.</div>`,
			totalFiles)
	}
	return buf.String()
}

// RenderFile renders one File. Public so callers (the diff-fragment
// endpoint) can re-render a single file on demand.
func RenderFile(f *parse.File, opts Options) string {
	opts = defaults(opts)
	var buf bytes.Buffer
	buf.WriteString(`<section class="shithub-diff-file">`)
	buf.WriteString(renderHeader(f))
	switch {
	case f.IsBinary:
		buf.WriteString(renderBinary(f))
	case f.IsTooLarge:
		buf.WriteString(renderTooLarge(f))
	case opts.Mode == ModeSplit:
		buf.WriteString(renderHunksSplit(f, opts))
	default:
		buf.WriteString(renderHunksUnified(f, opts))
	}
	buf.WriteString(`</section>`)
	return buf.String()
}

func renderHeader(f *parse.File) string {
	var label, action string
	switch {
	case f.IsRename:
		label = fmt.Sprintf("%s → %s", f.OldPath, f.NewPath)
		action = fmt.Sprintf("renamed (%d%% similarity)", f.Score)
	case f.IsCopy:
		label = fmt.Sprintf("%s → %s", f.OldPath, f.NewPath)
		action = fmt.Sprintf("copied (%d%% similarity)", f.Score)
	case f.IsNew:
		label = f.NewPath
		action = "added"
	case f.IsDelete:
		label = f.OldPath
		action = "deleted"
	default:
		label = f.NewPath
		if label == "" {
			label = f.OldPath
		}
		action = "modified"
	}
	return fmt.Sprintf(
		`<header class="shithub-diff-file-head"><code>%s</code><span class="shithub-diff-file-action">%s</span></header>`,
		html.EscapeString(label), html.EscapeString(action),
	)
}

func renderBinary(f *parse.File) string {
	if isImageExt(f.NewPath) || isImageExt(f.OldPath) {
		return renderImage(f)
	}
	return `<div class="shithub-diff-binary">Binary file changed.</div>`
}

func renderImage(f *parse.File) string {
	// Both old/new image previews — these resolve via the existing /raw
	// route, but we don't have ref context here, so the renderer emits
	// placeholder URLs the caller can substitute later. For the v1
	// commit-page integration the caller provides absolute /raw URLs
	// before calling Diff (or simply skips image preview). Keep the
	// surface simple: a placeholder note.
	return `<div class="shithub-diff-image">Image file changed (preview rendering wires once /raw URLs are threaded into the diff renderer).</div>`
}

func renderTooLarge(f *parse.File) string {
	lineCount := 0
	for _, h := range f.Hunks {
		lineCount += len(h.Lines)
	}
	return fmt.Sprintf(
		`<details class="shithub-diff-file-toolarge"><summary>%d lines changed — click to load</summary>%s</details>`,
		lineCount,
		// Lazy version: render hunks under the <details>. For a real
		// per-file fragment fetch (S19 spec) the summary would link to
		// /diff-fragment instead; keep the inline expand for now —
		// it's still safer than rendering above the fold.
		renderHunksUnified(f, Options{HighlightCap: 0}),
	)
}

func renderHunksUnified(f *parse.File, opts Options) string {
	var buf bytes.Buffer
	buf.WriteString(`<table class="shithub-diff-table shithub-diff-unified">`)
	for _, h := range f.Hunks {
		fmt.Fprintf(&buf, `<tr class="shithub-diff-hunk-head"><td colspan="3"><code>%s</code> <span>%s</span></td></tr>`,
			html.EscapeString(hunkHeader(h)),
			html.EscapeString(h.Section))
		for _, l := range h.Lines {
			old, neu := lineNoCells(l)
			content := highlightOrEscape(f.NewPath, l.Content, opts)
			fmt.Fprintf(
				&buf,
				`<tr class="%s"><td class="shithub-diff-lineno">%s</td><td class="shithub-diff-lineno">%s</td><td class="shithub-diff-content"><pre>%s%s</pre></td></tr>`,
				lineClass(l.Kind), old, neu, lineMarker(l.Kind), content,
			)
		}
	}
	buf.WriteString(`</table>`)
	return buf.String()
}

func renderHunksSplit(f *parse.File, opts Options) string {
	var buf bytes.Buffer
	buf.WriteString(`<table class="shithub-diff-table shithub-diff-split">`)
	for _, h := range f.Hunks {
		fmt.Fprintf(&buf, `<tr class="shithub-diff-hunk-head"><td colspan="4"><code>%s</code> <span>%s</span></td></tr>`,
			html.EscapeString(hunkHeader(h)),
			html.EscapeString(h.Section))
		// Pair adjacent +/- so the same row shows both sides. Context
		// lines fill both cells. Dangling adds/deletes pad the other.
		paired := pairLines(h.Lines)
		for _, p := range paired {
			leftClass := "shithub-diff-pad"
			rightClass := "shithub-diff-pad"
			leftNo := ""
			rightNo := ""
			leftContent := ""
			rightContent := ""
			if p.left != nil {
				leftClass = lineClass(p.left.Kind)
				leftNo = numStr(p.left.OldLineNo)
				leftContent = lineMarker(p.left.Kind) + highlightOrEscape(f.OldPath, p.left.Content, opts)
			}
			if p.right != nil {
				rightClass = lineClass(p.right.Kind)
				rightNo = numStr(p.right.NewLineNo)
				rightContent = lineMarker(p.right.Kind) + highlightOrEscape(f.NewPath, p.right.Content, opts)
			}
			fmt.Fprintf(
				&buf,
				`<tr><td class="shithub-diff-lineno %s">%s</td><td class="shithub-diff-content %s"><pre>%s</pre></td><td class="shithub-diff-lineno %s">%s</td><td class="shithub-diff-content %s"><pre>%s</pre></td></tr>`,
				leftClass, leftNo, leftClass, leftContent,
				rightClass, rightNo, rightClass, rightContent,
			)
		}
	}
	buf.WriteString(`</table>`)
	return buf.String()
}

// pair models one row of split-view: left side (old) and right side (new).
type pair struct {
	left, right *parse.Line
}

// pairLines walks a hunk's lines and aligns adds/deletes for the
// split view. The pairing is "row-by-row": consecutive deletes pair
// with consecutive adds in order; trailing additions on either side
// pad the other column. Context lines pair with themselves.
func pairLines(lines []parse.Line) []pair {
	var out []pair
	var pendingDel []parse.Line
	flush := func() {
		// Emit pendingDel as left-only rows.
		for i := range pendingDel {
			d := pendingDel[i]
			out = append(out, pair{left: &d})
		}
		pendingDel = pendingDel[:0]
	}
	for i := range lines {
		l := lines[i]
		switch l.Kind {
		case parse.LineDelete:
			pendingDel = append(pendingDel, l)
		case parse.LineAdd:
			if len(pendingDel) > 0 {
				d := pendingDel[0]
				pendingDel = pendingDel[1:]
				out = append(out, pair{left: &d, right: &l})
			} else {
				out = append(out, pair{right: &l})
			}
		default:
			flush()
			out = append(out, pair{left: &l, right: &l})
		}
	}
	flush()
	return out
}

func hunkHeader(h parse.Hunk) string {
	return fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
}

func lineClass(k parse.LineKind) string {
	switch k {
	case parse.LineAdd:
		return "shithub-diff-add"
	case parse.LineDelete:
		return "shithub-diff-del"
	case parse.LineNoNewline:
		return "shithub-diff-nonewline"
	default:
		return "shithub-diff-ctx"
	}
}

func lineMarker(k parse.LineKind) string {
	switch k {
	case parse.LineAdd:
		return "+"
	case parse.LineDelete:
		return "-"
	default:
		return " "
	}
}

func lineNoCells(l parse.Line) (string, string) {
	return numStr(l.OldLineNo), numStr(l.NewLineNo)
}

func numStr(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", n)
}

// highlightOrEscape returns syntax-highlighted HTML for the line, or
// HTML-escaped plain text when over the highlight cap (or filename is
// empty). Highlighting one line at a time lets the caller honor the
// per-file size cap without buffering the whole hunk.
func highlightOrEscape(filename, content string, opts Options) string {
	if opts.HighlightCap > 0 && len(content) > opts.HighlightCap {
		return html.EscapeString(content)
	}
	if filename == "" || filename == "/dev/null" {
		return html.EscapeString(content)
	}
	// Per-line chroma highlight is available via highlight.RenderLines
	// for callers that want token classes, but the diff renderer just
	// escapes for now — the page already has a "view source" link to
	// the highlighted blob.
	_ = highlight.RenderLines
	return html.EscapeString(content)
}

func isImageExt(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	}
	return false
}
