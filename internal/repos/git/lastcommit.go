// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DefaultLastCommitWalk bounds how many commits EntryLastCommits will
// walk before giving up on whatever is still unresolved. 2,000 commits
// of `git log --name-only` output streams in single-digit milliseconds
// on the boxes we run and covers the entire history of most
// directories; anything deeper is a directory whose files have not
// been touched in 2,000 commits, and the caller resolves those few
// stragglers with one targeted `git log -1 -- <path>` each.
const DefaultLastCommitWalk = 2000

// LastCommitOptions describes one "last commit per entry" resolution.
//
// Dir is the repo-relative directory being listed ("" for the root)
// and Names are its immediate children as `ls-tree` reported them
// (basenames, not full paths). Ref is any commit-ish; callers should
// pass the resolved commit OID when they intend to cache the result,
// so the cache key moves whenever history moves.
type LastCommitOptions struct {
	Ref        string
	Dir        string
	Names      []string
	MaxCommits int // 0 → DefaultLastCommitWalk
}

// EntryLastCommits resolves the most recent commit touching each named
// child of Dir using ONE `git log` walk instead of one `git log -1`
// per entry.
//
// The walk is `git log --name-only <ref> [-- <dir>]` read in reverse
// chronological order: the first time a path under `dir/<name>/`
// appears, `<name>`'s last commit is that commit. Once every requested
// name has an answer we kill git mid-stream rather than draining the
// rest of history, so the common case reads only as far back as the
// least-recently-touched entry in the directory.
//
// The returned map holds only the names that resolved. Entries missing
// from it were either never touched inside the walk bound, or live
// under a path shape git had to quote (embedded newline/quote); the
// caller falls back to a per-path `git log -1` for exactly those and
// so keeps byte-identical output to the N-fork version.
//
// A non-nil error still returns whatever resolved before the failure —
// partial results are usable, and the caller's per-path fallback
// covers the rest.
func EntryLastCommits(ctx context.Context, gitDir string, o LastCommitOptions) (map[string]Commit, error) {
	found := make(map[string]Commit, len(o.Names))
	want := make(map[string]struct{}, len(o.Names))
	for _, n := range o.Names {
		if n != "" {
			want[n] = struct{}{}
		}
	}
	if len(want) == 0 {
		return found, nil
	}

	maxCommits := o.MaxCommits
	if maxCommits <= 0 {
		maxCommits = DefaultLastCommitWalk
	}
	ref := o.Ref
	if ref == "" {
		ref = "HEAD"
	}

	ctx, cancel := readCtx(ctx)
	defer cancel()
	// A second, manually-triggered cancel so we can stop git the moment
	// the last entry resolves without waiting on the read deadline.
	walkCtx, stop := context.WithCancel(ctx)
	defer stop()

	args := []string{
		"-C", gitDir,
		// Keep non-ASCII paths verbatim so they compare equal to the
		// names ls-tree handed us.
		"-c", "core.quotePath=false",
		"log",
		"--max-count=" + strconv.Itoa(maxCommits),
		"--name-only",
		// Rename detection would only re-label a path we already see
		// under its new name, and it costs real CPU on large commits.
		"--no-renames",
		"--format=" + lastCommitFormat,
		ref,
	}
	if o.Dir != "" {
		args = append(args, "--", o.Dir)
	}

	cmd := gitCmd(walkCtx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return found, err
	}
	if err := cmd.Start(); err != nil {
		return found, err
	}

	prefix := ""
	if o.Dir != "" {
		prefix = o.Dir + "/"
	}

	var (
		cur      Commit
		haveCur  bool
		complete bool
	)
	sc := bufio.NewScanner(stdout)
	// Paths and subjects are short, but a pathological commit subject
	// shouldn't abort the walk with ErrTooLong.
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, lastCommitRecordSep) {
			cur, haveCur = parseLastCommitHeader(line[len(lastCommitRecordSep):])
			continue
		}
		if line == "" || !haveCur {
			continue
		}
		name, ok := immediateChild(prefix, line)
		if !ok {
			continue
		}
		if _, wanted := want[name]; !wanted {
			continue
		}
		found[name] = cur
		delete(want, name)
		if len(want) == 0 {
			complete = true
			break
		}
	}
	scanErr := sc.Err()
	// Kill git before reaping: when we broke out early it is still
	// writing into a pipe nobody reads, and Wait alone would block.
	stop()
	waitErr := cmd.Wait()

	if complete {
		// Early exit makes git's exit status meaningless (we killed it).
		return found, nil
	}
	if scanErr != nil {
		return found, fmt.Errorf("git log --name-only: %w", scanErr)
	}
	if waitErr != nil {
		return found, fmt.Errorf("git log --name-only: %w: %s",
			waitErr, strings.TrimSpace(stderr.String()))
	}
	return found, nil
}

// lastCommitRecordSep (ASCII record separator) opens every commit
// header line so the scanner can tell headers from the `--name-only`
// path lines that follow them. Fields inside the header are split on
// ASCII unit separator, matching the rest of this package.
const (
	lastCommitRecordSep = "\x1e"
	lastCommitFieldSep  = "\x1f"
	lastCommitFormat    = lastCommitRecordSep + "%H" + lastCommitFieldSep + "%h" +
		lastCommitFieldSep + "%an" + lastCommitFieldSep + "%ae" +
		lastCommitFieldSep + "%at" + lastCommitFieldSep + "%s"
)

// parseLastCommitHeader unpacks one header line. Body is intentionally
// left empty — the tree listing renders subjects only, and carrying
// bodies would multiply the cached payload for no rendered output.
func parseLastCommitHeader(line string) (Commit, bool) {
	parts := strings.SplitN(line, lastCommitFieldSep, 6)
	if len(parts) != 6 {
		return Commit{}, false
	}
	ts, err := strconv.ParseInt(parts[4], 10, 64)
	if err != nil {
		return Commit{}, false
	}
	return Commit{
		OID:         parts[0],
		ShortOID:    parts[1],
		AuthorName:  parts[2],
		AuthorEmail: parts[3],
		AuthorWhen:  time.Unix(ts, 0).UTC(),
		Subject:     parts[5],
	}, true
}

// immediateChild maps a full repo-relative path from `--name-only` to
// the name of the listed directory's immediate child that contains it.
// Paths outside the listed directory return ok=false.
func immediateChild(prefix, p string) (string, bool) {
	if prefix != "" {
		if !strings.HasPrefix(p, prefix) {
			return "", false
		}
		p = p[len(prefix):]
	}
	if i := strings.IndexByte(p, '/'); i >= 0 {
		p = p[:i]
	}
	if p == "" {
		return "", false
	}
	return p, true
}
