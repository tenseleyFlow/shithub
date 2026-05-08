// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// BlameLine is one source line + the commit that last touched it.
type BlameLine struct {
	OID         string
	ShortOID    string
	AuthorName  string
	AuthorEmail string
	AuthorWhen  time.Time
	OrigLineNo  int    // line number in the originating commit
	FinalLineNo int    // line number in the final file (1-indexed)
	Content     string // the source line, no trailing newline
}

// BlameChunk groups consecutive lines that share an originating commit.
// The handler renders each chunk with one header (commit/author/age) +
// N body rows.
type BlameChunk struct {
	OID         string
	ShortOID    string
	AuthorName  string
	AuthorEmail string
	AuthorWhen  time.Time
	StartLine   int // first FinalLineNo in the chunk
	EndLine     int
	Lines       []BlameLine
}

// BlameOptions configures Blame. Currently just the file ref + path,
// but the shape leaves room for "older" navigation in a follow-up.
type BlameOptions struct {
	Ref  string
	Path string

	// MaxBytes / MaxLines are the safety caps. 0 → use defaults
	// (5 MiB / 50k lines per S18 spec).
	MaxBytes int64
	MaxLines int
}

// ErrBlameTooLarge is returned when the file exceeds the cap. The
// handler renders the "blame too expensive" placeholder.
var ErrBlameTooLarge = errors.New("git: blame target too large")

// ErrBlameOnBinary is returned when the requested path is a binary
// blob. Blame on binary is meaningless; refuse early.
var ErrBlameOnBinary = errors.New("git: blame target is binary")

// Blame runs `git blame --line-porcelain` and returns chunks. The
// caller groups by chunk for rendering. We size-check the blob via
// StatPath first so a giant file fails fast without forking blame.
func Blame(ctx context.Context, gitDir string, opts BlameOptions) ([]BlameChunk, error) {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 5 * 1024 * 1024
	}
	maxLines := opts.MaxLines
	if maxLines <= 0 {
		maxLines = 50000
	}

	kind, _, size, err := StatPath(ctx, gitDir, opts.Ref, opts.Path)
	if err != nil {
		return nil, err
	}
	if kind != EntryBlob {
		return nil, ErrBlameOnBinary
	}
	if size > maxBytes {
		return nil, ErrBlameTooLarge
	}

	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"blame", "--line-porcelain", opts.Ref, "--", opts.Path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Wait() }()

	lines, err := parseLinePorcelain(stdout, maxLines)
	if err != nil {
		return nil, err
	}
	return groupBlame(lines), nil
}

// parseLinePorcelain consumes git's `--line-porcelain` output. The
// shape is: per source line a header block (10–15 metadata lines)
// followed by `\t<content>`. Headers we care about:
//
//	<oid> <orig_lineno> <final_lineno> [<groupsize>]
//	author <name>
//	author-mail <<email>>
//	author-time <unix>
//	(plus committer fields, summary, etc. — ignored)
//	\t<content>
//
// We stream-parse so a 50k-line blame doesn't sit in memory more than
// the working set (one BlameLine per finalLine).
func parseLinePorcelain(r interface{ Read(p []byte) (int, error) }, maxLines int) ([]BlameLine, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var (
		out []BlameLine
		cur BlameLine
	)
	flush := func() {
		// Trigger fires when we see a tab-prefixed content line; we
		// emit the current line then reset most fields. The OID etc.
		// repeat only on a new chunk header, so don't reset those.
	}
	_ = flush
	for sc.Scan() {
		raw := sc.Text()
		if strings.HasPrefix(raw, "\t") {
			cur.Content = raw[1:]
			out = append(out, cur)
			if len(out) > maxLines {
				return nil, ErrBlameTooLarge
			}
			continue
		}
		// Header line. The first header in a chunk is "<oid> <orig> <final>";
		// subsequent headers in the same chunk omit some fields. Subsequent
		// chunks repeat the SHA, so always parse SHA-leading lines.
		if isHexBlamePrefix(raw) {
			fields := strings.Fields(raw)
			if len(fields) >= 3 {
				cur.OID = fields[0]
				if len(cur.OID) >= 7 {
					cur.ShortOID = cur.OID[:7]
				}
				if n, err := strconv.Atoi(fields[1]); err == nil {
					cur.OrigLineNo = n
				}
				if n, err := strconv.Atoi(fields[2]); err == nil {
					cur.FinalLineNo = n
				}
			}
			continue
		}
		key, val, ok := cutHeader(raw)
		if !ok {
			continue
		}
		switch key {
		case "author":
			cur.AuthorName = val
		case "author-mail":
			// `<email>` form — strip angle brackets.
			cur.AuthorEmail = strings.Trim(val, "<>")
		case "author-time":
			if t, err := strconv.ParseInt(val, 10, 64); err == nil {
				cur.AuthorWhen = time.Unix(t, 0).UTC()
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("blame: scan: %w", err)
	}
	return out, nil
}

// cutHeader splits "key value" into ("key", "value", true). Returns
// false on malformed input so the caller can skip.
func cutHeader(s string) (string, string, bool) {
	idx := strings.IndexByte(s, ' ')
	if idx < 0 {
		return "", "", false
	}
	return s[:idx], s[idx+1:], true
}

// isHexBlamePrefix returns true when s starts with a 40-char hex blob
// followed by space — indicates this line is a chunk-or-line header
// rather than a `key value` field.
func isHexBlamePrefix(s string) bool {
	if len(s) < 41 || s[40] != ' ' {
		return false
	}
	return isHex(s[:40])
}

// groupBlame consolidates consecutive same-OID BlameLines into chunks.
// Chunks are emitted in line order — the renderer iterates them in
// sequence and renders one header + N rows per chunk.
func groupBlame(lines []BlameLine) []BlameChunk {
	if len(lines) == 0 {
		return nil
	}
	chunks := make([]BlameChunk, 0, 16)
	current := BlameChunk{
		OID:         lines[0].OID,
		ShortOID:    lines[0].ShortOID,
		AuthorName:  lines[0].AuthorName,
		AuthorEmail: lines[0].AuthorEmail,
		AuthorWhen:  lines[0].AuthorWhen,
		StartLine:   lines[0].FinalLineNo,
		EndLine:     lines[0].FinalLineNo,
		Lines:       []BlameLine{lines[0]},
	}
	for i := 1; i < len(lines); i++ {
		l := lines[i]
		if l.OID == current.OID {
			current.Lines = append(current.Lines, l)
			current.EndLine = l.FinalLineNo
			continue
		}
		chunks = append(chunks, current)
		current = BlameChunk{
			OID:         l.OID,
			ShortOID:    l.ShortOID,
			AuthorName:  l.AuthorName,
			AuthorEmail: l.AuthorEmail,
			AuthorWhen:  l.AuthorWhen,
			StartLine:   l.FinalLineNo,
			EndLine:     l.FinalLineNo,
			Lines:       []BlameLine{l},
		}
	}
	chunks = append(chunks, current)
	return chunks
}
