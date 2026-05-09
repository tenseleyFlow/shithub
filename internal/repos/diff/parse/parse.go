// SPDX-License-Identifier: AGPL-3.0-or-later

// Package parse wraps go-gitdiff. We expose a minimal struct surface
// (`Diff` / `File` / `Hunk` / `Line`) so the renderer doesn't depend
// on the upstream type names — keeps the option to swap libraries
// later if go-gitdiff evolves in an uncomfortable direction.
package parse

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// Diff is the top-level result of parsing a unified-diff stream.
type Diff struct {
	Files []File
}

// File is one changed path. OldPath / NewPath differ on rename/copy;
// they're equal on a normal modify. IsBinary, IsRename, IsCopy,
// IsNew, IsDelete summarize the extended-header flags so renderers
// don't reach into the upstream types.
type File struct {
	OldPath  string
	NewPath  string
	OldMode  string
	NewMode  string
	IsBinary bool
	IsNew    bool
	IsDelete bool
	IsRename bool
	IsCopy   bool
	Hunks    []Hunk
	// Score is the rename/copy similarity (0–100) when IsRename or IsCopy.
	Score int
	// IsTooLarge marks files past the per-file render threshold; the
	// renderer emits a "too large" placeholder + expander link rather
	// than the full hunks.
	IsTooLarge bool
	// SizeBytes is the rough cost of this file's hunks (sum of line
	// content). Used to drive truncation decisions.
	SizeBytes int
}

// Hunk is one `@@ -X,Y +A,B @@` block.
type Hunk struct {
	OldStart int
	OldLines int
	NewStart int
	NewLines int
	Section  string // text after the closing `@@` (function name etc.)
	Lines    []Line
}

// LineKind is the per-line context indicator.
type LineKind int

const (
	LineContext LineKind = iota
	LineAdd
	LineDelete
	LineNoNewline // "\ No newline at end of file"
)

// Line is one rendered row in a hunk.
type Line struct {
	Kind      LineKind
	OldLineNo int    // 0 when the line is a pure addition
	NewLineNo int    // 0 when the line is a pure deletion
	Content   string // without the leading +/-/space marker, no trailing newline
}

// Parse consumes a unified diff stream into the typed shape. The
// readSrc may be a bytes.Reader or any other io.Reader; we don't
// enforce a max-size cap here — caller decides.
func Parse(r io.Reader) (*Diff, error) {
	files, _, err := gitdiff.Parse(r)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse: %w", err)
	}
	out := &Diff{Files: make([]File, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, fromGitdiff(f))
	}
	return out, nil
}

// ParseBytes is a convenience for the common case of having the patch
// as a single buffer.
func ParseBytes(b []byte) (*Diff, error) {
	return Parse(bytes.NewReader(b))
}

// fromGitdiff translates the upstream type to ours. Hunks are walked
// in-place; we don't retain the upstream pointer so the renderer can
// be tested without importing gitdiff.
func fromGitdiff(f *gitdiff.File) File {
	out := File{
		OldPath:  f.OldName,
		NewPath:  f.NewName,
		IsBinary: f.IsBinary,
		IsNew:    f.IsNew,
		IsDelete: f.IsDelete,
		IsRename: f.IsRename,
		IsCopy:   f.IsCopy,
		Score:    f.Score,
	}
	if f.OldMode != 0 {
		out.OldMode = fmt.Sprintf("%06o", f.OldMode)
	}
	if f.NewMode != 0 {
		out.NewMode = fmt.Sprintf("%06o", f.NewMode)
	}
	if f.IsBinary {
		return out // hunks are absent for binary
	}

	hunks := make([]Hunk, 0, len(f.TextFragments))
	var totalBytes int
	for _, frag := range f.TextFragments {
		h := Hunk{
			OldStart: int(frag.OldPosition),
			OldLines: int(frag.OldLines),
			NewStart: int(frag.NewPosition),
			NewLines: int(frag.NewLines),
			Section:  frag.Comment,
		}
		oldNo := h.OldStart
		newNo := h.NewStart
		for _, gl := range frag.Lines {
			line := Line{Content: trimNewline(gl.Line)}
			totalBytes += len(line.Content)
			switch gl.Op {
			case gitdiff.OpContext:
				line.Kind = LineContext
				line.OldLineNo = oldNo
				line.NewLineNo = newNo
				oldNo++
				newNo++
			case gitdiff.OpAdd:
				line.Kind = LineAdd
				line.NewLineNo = newNo
				newNo++
			case gitdiff.OpDelete:
				line.Kind = LineDelete
				line.OldLineNo = oldNo
				oldNo++
			}
			h.Lines = append(h.Lines, line)
		}
		hunks = append(hunks, h)
	}
	out.Hunks = hunks
	out.SizeBytes = totalBytes
	return out
}

// trimNewline strips a single trailing `\n`. We deliberately don't
// touch the "\ No newline at end of file" marker (gitdiff materializes
// that as a separate line in our caller's loop — it isn't reached
// because go-gitdiff parses it as a special marker on the Op level
// only when present).
func trimNewline(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		return s[:n-1]
	}
	return s
}
