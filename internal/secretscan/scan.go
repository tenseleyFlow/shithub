// SPDX-License-Identifier: AGPL-3.0-or-later

package secretscan

import (
	"bytes"
	"strings"
)

// Finding is one detected occurrence of a pattern in scanned content.
// Excerpt is REDACTED — the raw secret value never leaves this
// package. Worker storage rows persist (Pattern, Path, Line) but not
// the matched bytes (10b will pin this in the migration).
type Finding struct {
	// Pattern is the registered Pattern.Name. Stable across releases.
	Pattern string
	// Line is 1-indexed. The whole line is enough localization for the
	// allowlist UX (path+line tuple); we don't expose a column to avoid
	// telegraphing the exact span of the secret.
	Line int
	// Excerpt is a redacted snippet showing the surrounding code with
	// the matched bytes replaced by `[REDACTED]`. Useful for context in
	// the UI without leaking the credential itself.
	Excerpt string
}

// ScanOptions controls per-call knobs. Zero value is the production
// default (full pattern set, all paths considered).
type ScanOptions struct {
	// Patterns, when non-nil, replaces the package-level Patterns set
	// for this call. Tests use this to isolate a single pattern.
	Patterns []Pattern
	// MaxBytes caps the size of content scanned. Larger inputs are
	// truncated (the trailing portion is dropped). Zero means no
	// cap — the worker will set a reasonable cap to bound CPU.
	MaxBytes int
}

// Scan walks `content` for every registered pattern and returns one
// Finding per match. Content is treated as bytes; no encoding
// assumptions. Lines are split on \n (\r\n is normalized).
//
// The function is allocation-conservative and safe for concurrent
// use. It does NOT mutate the input.
func Scan(content []byte, opts ScanOptions) []Finding {
	if len(content) == 0 {
		return nil
	}
	if opts.MaxBytes > 0 && len(content) > opts.MaxBytes {
		content = content[:opts.MaxBytes]
	}
	patterns := opts.Patterns
	if patterns == nil {
		patterns = Patterns
	}
	// Pre-compute line starts so we can map an offset to a 1-indexed
	// line cheaply. lineStarts[i] is the byte offset where line (i+1)
	// begins. Line 1 always starts at 0; subsequent entries are the
	// byte after each '\n'.
	lineStarts := []int{0}
	for i, b := range content {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	out := make([]Finding, 0)
	for _, p := range patterns {
		matches := p.Re.FindAllIndex(content, -1)
		for _, m := range matches {
			if p.MinMatchLen > 0 && (m[1]-m[0]) < p.MinMatchLen {
				continue
			}
			line := lineForOffset(lineStarts, m[0])
			out = append(out, Finding{
				Pattern: p.Name,
				Line:    line,
				Excerpt: redactLine(content, lineStarts, line, m[0], m[1]),
			})
		}
	}
	return out
}

// lineForOffset binary-searches the lineStarts array for the largest
// entry <= off; the index is the 0-based line, so we return idx+1.
func lineForOffset(lineStarts []int, off int) int {
	lo, hi := 0, len(lineStarts)
	for lo < hi {
		mid := (lo + hi) / 2
		if lineStarts[mid] <= off {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// redactLine returns the matched line (1-indexed) with bytes between
// matchStart and matchEnd replaced by `[REDACTED]`. The line is
// trimmed to ExcerptMaxLineBytes so a one-character-per-line obfuscated
// secret in a 100 KB JSON blob doesn't ship a giant excerpt to the UI.
func redactLine(content []byte, lineStarts []int, line, matchStart, matchEnd int) string {
	const excerptMaxLineBytes = 160
	if line < 1 {
		return ""
	}
	startOff := lineStarts[line-1]
	var endOff int
	if line < len(lineStarts) {
		endOff = lineStarts[line] - 1 // strip the trailing '\n'
	} else {
		endOff = len(content)
	}
	if endOff < startOff {
		return ""
	}
	// Bound matchStart/matchEnd to the line so a pattern spanning
	// multiple lines (the private-key header doesn't, but defense in
	// depth) still produces a sensible single-line excerpt.
	mStart := matchStart
	if mStart < startOff {
		mStart = startOff
	}
	if mStart > endOff {
		mStart = endOff
	}
	mEnd := matchEnd
	if mEnd > endOff {
		mEnd = endOff
	}
	if mEnd < mStart {
		mEnd = mStart
	}

	pre := content[startOff:mStart]
	post := content[mEnd:endOff]
	// Strip CR if present at end (Windows line endings).
	post = bytes.TrimRight(post, "\r")

	var b strings.Builder
	b.Grow(len(pre) + len("[REDACTED]") + len(post))
	b.Write(pre)
	b.WriteString("[REDACTED]")
	b.Write(post)
	s := b.String()
	if len(s) > excerptMaxLineBytes {
		// Centre on the redaction marker so the user sees the
		// surrounding context rather than the start of an indented
		// line.
		idx := strings.Index(s, "[REDACTED]")
		if idx < 0 {
			return s[:excerptMaxLineBytes]
		}
		halfWindow := (excerptMaxLineBytes - len("[REDACTED]")) / 2
		left := idx - halfWindow
		if left < 0 {
			left = 0
		}
		right := idx + len("[REDACTED]") + halfWindow
		if right > len(s) {
			right = len(s)
		}
		ell := ""
		if left > 0 {
			ell = "…"
		}
		tail := ""
		if right < len(s) {
			tail = "…"
		}
		return ell + s[left:right] + tail
	}
	return s
}
