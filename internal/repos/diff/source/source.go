// SPDX-License-Identifier: AGPL-3.0-or-later

// Package source produces raw git-diff patch bytes for the parser.
// Three flavors:
//
//   - FromCommit  — commit-vs-parent (for the single-commit page)
//   - FromRange   — base..head two-dot (for compare-style diffs)
//   - FromMergeBase — base...head three-dot (for PR / compare against
//     the merge-base; matches GitHub's PR file-list behavior)
//
// Each function returns the unified-diff bytes; the parser package
// consumes from there. Whitespace-ignoring requested via Options.
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Options tunes a diff source. IgnoreWhitespace passes -w to git so
// whitespace-only lines vanish from the patch (cleaner UX than parser-
// side filtering, and git's whitespace handling respects language
// quirks like indentation-sensitive Python).
type Options struct {
	IgnoreWhitespace bool
	// FindRenames toggles -M (--find-renames). Default off matches
	// historical git behavior; the package's helpers turn it on for
	// the rendered surfaces because rename detection is what users
	// expect on a code-review page.
	FindRenames bool
}

// FromCommit returns the diff between sha and its first parent. For a
// root commit (no parent) we diff against the empty tree by using
// `git diff-tree -p -r --root`.
func FromCommit(ctx context.Context, gitDir, sha string, opts Options) ([]byte, error) {
	args := []string{
		"-C", gitDir,
		"diff-tree", "-p", "-r", "--root",
		"--no-color", "--no-ext-diff",
		"--full-index",
	}
	args = append(args, opts.gitFlags()...)
	args = append(args, sha)
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, fmtErr("FromCommit", err)
	}
	return stripFirstHeader(out), nil
}

// FromRange returns the two-dot diff base..head. Use for "show me
// every change between these two refs", regardless of merge graph.
func FromRange(ctx context.Context, gitDir, base, head string, opts Options) ([]byte, error) {
	args := []string{
		"-C", gitDir,
		"diff", "--patch",
		"--no-color", "--no-ext-diff",
		"--full-index",
	}
	args = append(args, opts.gitFlags()...)
	args = append(args, base+".."+head, "--")
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, fmtErr("FromRange", err)
	}
	return out, nil
}

// FromMergeBase returns the three-dot diff base...head. Equivalent to
// `git diff $(git merge-base base head)..head`. Used by PR / compare
// pages — shows only what `head` adds over the common ancestor.
func FromMergeBase(ctx context.Context, gitDir, base, head string, opts Options) ([]byte, error) {
	args := []string{
		"-C", gitDir,
		"diff", "--patch",
		"--no-color", "--no-ext-diff",
		"--full-index",
	}
	args = append(args, opts.gitFlags()...)
	args = append(args, base+"..."+head, "--")
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return nil, fmtErr("FromMergeBase", err)
	}
	return out, nil
}

// gitFlags translates Options to the git-cli flags that the three
// helpers share.
func (o Options) gitFlags() []string {
	var flags []string
	if o.IgnoreWhitespace {
		flags = append(flags, "-w")
	}
	if o.FindRenames {
		flags = append(flags, "-M", "-C")
	}
	return flags
}

// stripFirstHeader removes the leading "<sha>\n" line that
// `git diff-tree` emits before the first patch hunk. The parser
// expects to start at "diff --git ..."; the leading SHA line confuses
// it (or at minimum produces a spurious empty file entry).
func stripFirstHeader(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	idx := bytes.IndexByte(b, '\n')
	if idx < 0 {
		return b
	}
	first := b[:idx]
	// The leading line is just the SHA — 40 hex chars.
	if len(first) == 40 && allHex(first) {
		return b[idx+1:]
	}
	return b
}

func allHex(b []byte) bool {
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// fmtErr wraps an exec error with stderr context when available.
func fmtErr(op string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%s: %w: %s", op, err, ee.Stderr)
	}
	return fmt.Errorf("%s: %w", op, err)
}
