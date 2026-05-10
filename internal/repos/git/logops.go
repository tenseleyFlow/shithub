// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Commit is one commit row from the commits list. Only the cheap fields
// arrive via `git log --format=...`; the per-file stats live on
// CommitDetail.
type Commit struct {
	OID         string // full 40-char SHA
	ShortOID    string // git's --abbrev result, typically 7 chars
	AuthorName  string
	AuthorEmail string
	AuthorWhen  time.Time
	Subject     string
	Body        string // empty for list view; populated by GetCommit
}

// LogOptions tunes the commits-list query. Zero values are sensible:
//   - MaxCount 0 → 30
//   - Skip 0 → 0
//   - Path "" → log over all paths
//   - Author "" → no author filter
//   - Since/Until zero → no date filter
type LogOptions struct {
	Ref      string
	MaxCount int
	Skip     int
	Path     string
	Author   string
	Since    time.Time
	Until    time.Time
	Follow   bool // --follow; only meaningful when Path is set
}

// Log returns Commits for the requested page. The format string packs
// every field we render onto one line per commit, separated by ASCII
// unit-separators (\x1f), with body terminated by ASCII record-separator
// (\x1e) so newlines inside the body don't break parsing.
func Log(ctx context.Context, gitDir string, o LogOptions) ([]Commit, error) {
	if o.MaxCount <= 0 {
		o.MaxCount = 30
	}
	const sep = "\x1f"
	const recordEnd = "\x1e"
	// %H %h %an %ae %at %s %b — body last so embedded \x1f in body
	// don't confuse SplitN.
	format := strings.Join([]string{"%H", "%h", "%an", "%ae", "%at", "%s"}, sep) + sep + "%b" + recordEnd

	args := []string{
		"-C", gitDir, "log",
		"--max-count=" + strconv.Itoa(o.MaxCount),
		"--skip=" + strconv.Itoa(o.Skip),
		"--format=" + format,
	}
	if o.Author != "" {
		args = append(args, "--author="+o.Author)
	}
	if !o.Since.IsZero() {
		args = append(args, "--since="+o.Since.UTC().Format(time.RFC3339))
	}
	if !o.Until.IsZero() {
		args = append(args, "--until="+o.Until.UTC().Format(time.RFC3339))
	}
	args = append(args, o.Ref)
	if o.Path != "" {
		if o.Follow {
			args = append(args, "--follow")
		}
		args = append(args, "--", o.Path)
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}
	return parseLogOutput(out)
}

// CountCommits returns the number of commits reachable from ref.
func CountCommits(ctx context.Context, gitDir, ref string) (int, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-list", "--count", ref)
	out, err := cmd.Output()
	if err != nil {
		return 0, wrapExecErr(err)
	}
	count, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("git rev-list count: %w", err)
	}
	return count, nil
}

// WeeklyCommitActivity counts commits by UTC week, oldest bucket first.
// It is intentionally small and read-only so list surfaces can render
// GitHub-style activity sparklines without parsing full commit rows.
func WeeklyCommitActivity(ctx context.Context, gitDir, ref string, bucketCount int, now time.Time) ([]int, error) {
	if bucketCount <= 0 {
		bucketCount = 52
	}
	if ref == "" {
		ref = "HEAD"
	}
	if now.IsZero() {
		now = time.Now()
	}
	weekStart := startOfUTCWeek(now.UTC())
	start := weekStart.AddDate(0, 0, -7*(bucketCount-1))
	end := weekStart.AddDate(0, 0, 7)

	//nolint:gosec // G204: gitDir is constrained by RepoFS path validation; ref is an argv value.
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "log",
		"--format=%ct",
		"--since="+start.Format(time.RFC3339),
		"--until="+end.Format(time.RFC3339),
		ref,
		"--",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}

	buckets := make([]int, bucketCount)
	for _, raw := range strings.Fields(string(out)) {
		ts, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			continue
		}
		when := time.Unix(ts, 0).UTC()
		idx := int(when.Sub(start) / (7 * 24 * time.Hour))
		if idx >= 0 && idx < bucketCount {
			buckets[idx]++
		}
	}
	return buckets, nil
}

func startOfUTCWeek(t time.Time) time.Time {
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	offset := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -offset)
}

// parseLogOutput unpacks the format above into Commits. Stable: the
// recordEnd lets us split records first, then unpack each.
func parseLogOutput(out []byte) ([]Commit, error) {
	const sep = "\x1f"
	const recordEnd = "\x1e"
	body := bytes.TrimRight(out, "\n")
	records := bytes.Split(body, []byte(recordEnd))
	commits := make([]Commit, 0, len(records))
	for _, rec := range records {
		rec = bytes.TrimLeft(rec, "\n")
		if len(rec) == 0 {
			continue
		}
		parts := strings.SplitN(string(rec), sep, 7)
		if len(parts) < 7 {
			continue
		}
		ts, _ := strconv.ParseInt(parts[4], 10, 64)
		commits = append(commits, Commit{
			OID:         parts[0],
			ShortOID:    parts[1],
			AuthorName:  parts[2],
			AuthorEmail: parts[3],
			AuthorWhen:  time.Unix(ts, 0).UTC(),
			Subject:     parts[5],
			Body:        strings.TrimSpace(parts[6]),
		})
	}
	return commits, nil
}

// CommitDetail holds the single-commit-view data: the Commit plus
// committer fields, parents, tree OID, and the per-file change list.
type CommitDetail struct {
	Commit
	CommitterName  string
	CommitterEmail string
	CommitterWhen  time.Time
	Parents        []string // each is a full OID
	TreeOID        string
	Files          []FileChange
}

// FileChange is one row from `git diff-tree --numstat --no-commit-id -r`.
// Status is git's letter code: A added, M modified, D deleted, R renamed,
// C copied, T type-changed.
type FileChange struct {
	Status  string
	Path    string
	OldPath string // populated for R/C
	Insert  int
	Delete  int
	Binary  bool
}

// GetCommit returns the full detail for one SHA. Two commands: one for
// the commit metadata + parents (cheap, single-line), one for the file
// stats. Combining them in one --pretty walk would save a fork but
// complicate parsing.
func GetCommit(ctx context.Context, gitDir, sha string) (CommitDetail, error) {
	const sep = "\x1f"
	// %H %h %an %ae %at %cn %ce %ct %P %T %s\n%b — single record.
	format := strings.Join([]string{
		"%H", "%h", "%an", "%ae", "%at",
		"%cn", "%ce", "%ct", "%P", "%T", "%s",
	}, sep) + sep + "%B"

	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"log", "-1", "--format="+format, sha, "--")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && bytes.Contains(ee.Stderr, []byte("unknown revision")) {
			return CommitDetail{}, ErrCommitNotFound
		}
		return CommitDetail{}, wrapExecErr(err)
	}
	parts := strings.SplitN(string(bytes.TrimRight(out, "\n")), sep, 12)
	if len(parts) < 12 {
		return CommitDetail{}, fmt.Errorf("git log: malformed output: %q", string(out))
	}
	at, _ := strconv.ParseInt(parts[4], 10, 64)
	ct, _ := strconv.ParseInt(parts[7], 10, 64)
	parentList := []string{}
	if pp := strings.TrimSpace(parts[8]); pp != "" {
		parentList = strings.Fields(pp)
	}

	// %B is "subject + blank + body"; we already have Subject from %s.
	rawMessage := parts[11]
	body := strings.TrimSpace(strings.TrimPrefix(rawMessage, parts[10]))

	cd := CommitDetail{
		Commit: Commit{
			OID:         parts[0],
			ShortOID:    parts[1],
			AuthorName:  parts[2],
			AuthorEmail: parts[3],
			AuthorWhen:  time.Unix(at, 0).UTC(),
			Subject:     parts[10],
			Body:        body,
		},
		CommitterName:  parts[5],
		CommitterEmail: parts[6],
		CommitterWhen:  time.Unix(ct, 0).UTC(),
		Parents:        parentList,
		TreeOID:        parts[9],
	}

	files, err := DiffStat(ctx, gitDir, sha)
	if err != nil {
		return cd, fmt.Errorf("diff-stat: %w", err)
	}
	cd.Files = files
	return cd, nil
}

// ErrCommitNotFound is returned by GetCommit when the SHA doesn't
// resolve to a commit on this repo.
var ErrCommitNotFound = errors.New("git: commit not found")

// DiffStat returns the per-file change list for a SHA. We run two
// commands: --name-status for the letter and rename pairs, --numstat
// for +/- counts. Two passes is two forks, but the parsing stays
// simple; combining via --raw is harder to read.
func DiffStat(ctx context.Context, gitDir, sha string) ([]FileChange, error) {
	// --name-status produces: "M\tpath" or "R100\told\tnew" etc.
	// `--root` makes the initial (parentless) commit show its files
	// against the empty tree; without it diff-tree emits nothing for
	// root commits.
	nsOut, err := exec.CommandContext(ctx, "git", "-C", gitDir,
		"diff-tree", "-r", "--root", "--name-status", "--no-commit-id", "-M", "-C", sha).Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}
	// --numstat: "<add>\t<del>\t<path>" or "-\t-\t<path>" for binary.
	numOut, err := exec.CommandContext(ctx, "git", "-C", gitDir,
		"diff-tree", "-r", "--root", "--numstat", "--no-commit-id", "-M", "-C", sha).Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}

	type ns struct {
		status, oldPath, path string
	}
	var nsRows []ns
	for _, line := range strings.Split(strings.TrimRight(string(nsOut), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 2 {
			continue
		}
		row := ns{status: fields[0]}
		// Rename/copy: status starts with R or C followed by a similarity
		// number; field layout is "Rxxx\tOLD\tNEW" or "Cxxx\tOLD\tNEW".
		if (strings.HasPrefix(row.status, "R") || strings.HasPrefix(row.status, "C")) && len(fields) == 3 {
			row.oldPath = fields[1]
			row.path = fields[2]
			row.status = string(row.status[0])
		} else {
			row.path = fields[len(fields)-1]
		}
		nsRows = append(nsRows, row)
	}

	type stat struct {
		ins, del int
		binary   bool
	}
	statByPath := make(map[string]stat, len(nsRows))
	for _, line := range strings.Split(strings.TrimRight(string(numOut), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		path := fields[len(fields)-1]
		if fields[0] == "-" {
			statByPath[path] = stat{binary: true}
			continue
		}
		ins, _ := strconv.Atoi(fields[0])
		del, _ := strconv.Atoi(fields[1])
		statByPath[path] = stat{ins: ins, del: del}
	}

	out := make([]FileChange, 0, len(nsRows))
	for _, n := range nsRows {
		s := statByPath[n.path]
		out = append(out, FileChange{
			Status: n.status, Path: n.path, OldPath: n.oldPath,
			Insert: s.ins, Delete: s.del, Binary: s.binary,
		})
	}
	return out, nil
}
