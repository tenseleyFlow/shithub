// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ResolveRefOID returns the full SHA for `ref` via `git rev-parse`.
// Returns ErrRefNotFound when git can't resolve.
func ResolveRefOID(ctx context.Context, gitDir, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir, "rev-parse", "--verify", ref+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", ErrRefNotFound
		}
		return "", wrapExecErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// MergeTreeResult captures the output of `git merge-tree --write-tree
// --merge-base=<base> <base> <head>`. ConflictPaths is empty when the
// merge is clean. Git ≥ 2.38 required.
type MergeTreeResult struct {
	TreeOID       string
	ConflictPaths []string
	HasConflict   bool
}

// ProbeMerge runs `git merge-tree --write-tree --no-messages <base>
// <head>` and lets git auto-compute the merge base. Exit 0 = clean
// merge (TreeOID set); exit 1 = conflicts (ConflictPaths populated).
// Anything else is wrapped.
func ProbeMerge(ctx context.Context, gitDir, baseOID, headOID string) (MergeTreeResult, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", gitDir,
		"merge-tree", "--write-tree", "--no-messages",
		baseOID, headOID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := strings.TrimRight(stdout.String(), "\n")
	if err == nil {
		return MergeTreeResult{TreeOID: out}, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		// First line is the tree OID even on conflict; subsequent lines
		// list conflicting paths (one per line) when --no-messages is set.
		lines := strings.Split(out, "\n")
		res := MergeTreeResult{HasConflict: true}
		if len(lines) > 0 {
			res.TreeOID = lines[0]
		}
		for _, l := range lines[1:] {
			if l = strings.TrimSpace(l); l != "" {
				res.ConflictPaths = append(res.ConflictPaths, l)
			}
		}
		return res, nil
	}
	return MergeTreeResult{}, fmt.Errorf("merge-tree: %w (%s)", err, stderr.String())
}

// CommitsBetweenDetail returns commits unique to head, with author +
// committer + body, suitable for refreshing pull_request_commits. The
// result preserves head-side oldest-first ordering by inverting log's
// default newest-first via --reverse.
func CommitsBetweenDetail(ctx context.Context, gitDir, baseOID, headOID string, max int) ([]CommitDetail, error) {
	if max <= 0 {
		max = 250
	}
	const sep = "\x1f"
	const recordEnd = "\x1e"
	format := strings.Join([]string{
		"%H", "%h",
		"%an", "%ae", "%at",
		"%cn", "%ce", "%ct",
		"%s",
	}, sep) + sep + "%b" + recordEnd
	cmd := exec.CommandContext(
		ctx, "git", "-C", gitDir,
		"log", "--reverse",
		"--max-count="+strconv.Itoa(max),
		"--format="+format,
		baseOID+".."+headOID,
	)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "unknown revision") {
			return nil, ErrRefNotFound
		}
		return nil, wrapExecErr(err)
	}
	return parseCommitDetail(out), nil
}

func parseCommitDetail(out []byte) []CommitDetail {
	const sep = "\x1f"
	const recordEnd = "\x1e"
	body := bytes.TrimRight(out, "\n")
	records := bytes.Split(body, []byte(recordEnd))
	cs := make([]CommitDetail, 0, len(records))
	for _, rec := range records {
		rec = bytes.TrimLeft(rec, "\n")
		if len(rec) == 0 {
			continue
		}
		parts := strings.SplitN(string(rec), sep, 10)
		if len(parts) < 10 {
			continue
		}
		ats, _ := strconv.ParseInt(parts[4], 10, 64)
		cts, _ := strconv.ParseInt(parts[7], 10, 64)
		cs = append(cs, CommitDetail{
			Commit: Commit{
				OID:         parts[0],
				ShortOID:    parts[1],
				AuthorName:  parts[2],
				AuthorEmail: parts[3],
				AuthorWhen:  time.Unix(ats, 0).UTC(),
				Subject:     parts[8],
				Body:        strings.TrimSpace(parts[9]),
			},
			CommitterName:  parts[5],
			CommitterEmail: parts[6],
			CommitterWhen:  time.Unix(cts, 0).UTC(),
		})
	}
	return cs
}

// FilesChangedBetween returns the change set head-side, computed as
// `git diff --name-status --numstat <base>...<head>` (three-dot:
// changes from merge-base to head). Status is git's letter code,
// renames carry the old path as the second column.
func FilesChangedBetween(ctx context.Context, gitDir, baseOID, headOID string) ([]PRFileChange, error) {
	cmd := exec.CommandContext(
		ctx, "git", "-C", gitDir,
		"diff", "--name-status", "-M", "-C",
		baseOID+"..."+headOID,
	)
	statusOut, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "unknown revision") {
			return nil, ErrRefNotFound
		}
		return nil, wrapExecErr(err)
	}
	cmd = exec.CommandContext(
		ctx, "git", "-C", gitDir,
		"diff", "--numstat", "-M", "-C",
		baseOID+"..."+headOID,
	)
	numOut, err := cmd.Output()
	if err != nil {
		return nil, wrapExecErr(err)
	}
	return parseFilesChanged(statusOut, numOut), nil
}

// PRFileChange is a per-file row for pull_request_files. Status mirrors
// the migration enum; OldPath is non-empty for renames + copies.
type PRFileChange struct {
	Path      string
	OldPath   string
	Status    string // "added" | "modified" | "deleted" | "renamed" | "copied"
	Additions int
	Deletions int
}

func parseFilesChanged(statusOut, numOut []byte) []PRFileChange {
	type stats struct{ adds, dels int }
	numByPath := map[string]stats{}
	for _, line := range strings.Split(string(numOut), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 3 {
			continue
		}
		// "-" appears for binary files; treat as 0/0 counts.
		a, _ := strconv.Atoi(fields[0])
		d, _ := strconv.Atoi(fields[1])
		// For renames/copies the path field is `old\x00new`-ish via {oldpath => newpath};
		// numstat's last field is just the new path when -M is applied.
		key := fields[len(fields)-1]
		numByPath[key] = stats{a, d}
	}
	out := []PRFileChange{}
	for _, line := range strings.Split(string(statusOut), "\n") {
		fields := strings.Split(strings.TrimSpace(line), "\t")
		if len(fields) < 2 {
			continue
		}
		statusLetter := fields[0]
		var fc PRFileChange
		switch {
		case strings.HasPrefix(statusLetter, "R") && len(fields) >= 3:
			fc.Status = "renamed"
			fc.OldPath = fields[1]
			fc.Path = fields[2]
		case strings.HasPrefix(statusLetter, "C") && len(fields) >= 3:
			fc.Status = "copied"
			fc.OldPath = fields[1]
			fc.Path = fields[2]
		case statusLetter == "A":
			fc.Status = "added"
			fc.Path = fields[1]
		case statusLetter == "D":
			fc.Status = "deleted"
			fc.Path = fields[1]
		case statusLetter == "M":
			fc.Status = "modified"
			fc.Path = fields[1]
		default:
			fc.Status = "modified"
			fc.Path = fields[len(fields)-1]
		}
		s := numByPath[fc.Path]
		fc.Additions = s.adds
		fc.Deletions = s.dels
		out = append(out, fc)
	}
	return out
}

// MergeOptions configures a worktree-based merge.
type MergeOptions struct {
	GitDir         string
	BaseRef        string // e.g. "refs/heads/trunk"
	BaseOID        string
	HeadOID        string
	Method         string // "merge" | "squash" | "rebase"
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	When           time.Time
	Subject        string
	Body           string
	WorktreesDir   string // parent dir for the temp worktree (must share volume with GitDir)
}

// MergeResult is what PerformMerge returns on success.
type MergeResult struct {
	NewBaseOID string // the new tip of base_ref after the merge
	MergedOID  string // for "merge" method: the merge commit; "squash"/"rebase": same as NewBaseOID
}

// PerformMerge executes the requested merge strategy in a temp worktree
// rooted at WorktreesDir. The worktree is removed on every exit path
// (success or failure). Returns the new base-ref tip.
func PerformMerge(ctx context.Context, opts MergeOptions) (MergeResult, error) {
	if opts.WorktreesDir == "" {
		opts.WorktreesDir = filepath.Join(filepath.Dir(opts.GitDir), ".tmp-worktrees")
	}
	if err := os.MkdirAll(opts.WorktreesDir, 0o750); err != nil {
		return MergeResult{}, fmt.Errorf("worktrees dir: %w", err)
	}
	wt, err := os.MkdirTemp(opts.WorktreesDir, "merge-*")
	if err != nil {
		return MergeResult{}, fmt.Errorf("mktemp worktree: %w", err)
	}
	cleanup := func() {
		// `git worktree remove --force` ignores stale entries; --force
		// also drops the directory contents so a leftover after a panic
		// gets reaped on the next attempt.
		_ = exec.Command("git", "-C", opts.GitDir, "worktree", "remove", "--force", wt).Run()
		_ = os.RemoveAll(wt)
	}
	defer cleanup()

	// Set up the worktree at base_oid (detached). Using detached HEAD
	// keeps the worktree from polluting the bare repo's branch refs;
	// we only push the resulting commit back to base_ref at the end.
	addCmd := exec.CommandContext(ctx, "git", "-C", opts.GitDir,
		"worktree", "add", "--detach", wt, opts.BaseOID)
	if out, err := addCmd.CombinedOutput(); err != nil {
		return MergeResult{}, fmt.Errorf("worktree add: %w (%s)", err, out)
	}

	// Identity for the merge commit. `--no-edit` + a baked subject
	// keeps the merge non-interactive.
	envBase := append(
		os.Environ(),
		"GIT_AUTHOR_NAME="+opts.AuthorName,
		"GIT_AUTHOR_EMAIL="+opts.AuthorEmail,
		"GIT_COMMITTER_NAME="+opts.CommitterName,
		"GIT_COMMITTER_EMAIL="+opts.CommitterEmail,
	)
	if !opts.When.IsZero() {
		envBase = append(
			envBase,
			"GIT_AUTHOR_DATE="+opts.When.Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+opts.When.Format(time.RFC3339),
		)
	}

	switch opts.Method {
	case "merge":
		// Non-fast-forward merge so we always get a merge commit, even
		// when the head is strictly ahead of base.
		msg := strings.TrimSpace(opts.Subject)
		if opts.Body != "" {
			msg += "\n\n" + opts.Body
		}
		mergeCmd := exec.CommandContext(ctx, "git", "-C", wt,
			"merge", "--no-ff", "--no-edit", "-m", msg, opts.HeadOID)
		mergeCmd.Env = envBase
		if out, err := mergeCmd.CombinedOutput(); err != nil {
			return MergeResult{}, fmt.Errorf("merge --no-ff: %w (%s)", err, out)
		}
	case "squash":
		// `git merge --squash` stages the squashed change without
		// committing; `git commit` makes the squash commit with a
		// single author/committer pair.
		squashCmd := exec.CommandContext(ctx, "git", "-C", wt,
			"merge", "--squash", opts.HeadOID)
		squashCmd.Env = envBase
		if out, err := squashCmd.CombinedOutput(); err != nil {
			return MergeResult{}, fmt.Errorf("merge --squash: %w (%s)", err, out)
		}
		msg := strings.TrimSpace(opts.Subject)
		if opts.Body != "" {
			msg += "\n\n" + opts.Body
		}
		commitCmd := exec.CommandContext(ctx, "git", "-C", wt,
			"commit", "-m", msg)
		commitCmd.Env = envBase
		if out, err := commitCmd.CombinedOutput(); err != nil {
			return MergeResult{}, fmt.Errorf("squash commit: %w (%s)", err, out)
		}
	case "rebase":
		// Replay head_oid onto base_oid. --rebase-merges off means we
		// flatten merge commits into linear history; this matches the
		// standard "rebase merge" UX.
		rebaseCmd := exec.CommandContext(ctx, "git", "-C", wt,
			"rebase", "--onto", opts.BaseOID, opts.BaseOID, opts.HeadOID)
		rebaseCmd.Env = envBase
		if out, err := rebaseCmd.CombinedOutput(); err != nil {
			// Best-effort abort so the worktree is reusable for the
			// cleanup step (cleanup deletes anyway, but keeps logs sane).
			_ = exec.Command("git", "-C", wt, "rebase", "--abort").Run()
			return MergeResult{}, fmt.Errorf("rebase --onto: %w (%s)", err, out)
		}
	default:
		return MergeResult{}, fmt.Errorf("unknown merge method %q", opts.Method)
	}

	// Capture the resulting tip of HEAD in the worktree.
	revOut, err := exec.CommandContext(ctx, "git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		return MergeResult{}, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	newOID := strings.TrimSpace(string(revOut))

	// Update base_ref atomically via update-ref, gated on the expected
	// old OID to defend against concurrent pushes during the merge.
	updateCmd := exec.CommandContext(ctx, "git", "-C", opts.GitDir,
		"update-ref", opts.BaseRef, newOID, opts.BaseOID)
	if out, err := updateCmd.CombinedOutput(); err != nil {
		return MergeResult{}, fmt.Errorf("update-ref %s: %w (%s)", opts.BaseRef, err, out)
	}

	// For "merge" method the merge commit is HEAD; for squash and
	// rebase it's the same as the new base tip.
	return MergeResult{NewBaseOID: newOID, MergedOID: newOID}, nil
}

// ErrBranchAlreadyUpToDate is returned by UpdateBranchFromBase when
// the head ref already contains the base tip in its history — the
// `pr update-branch` operation would be a no-op.
var ErrBranchAlreadyUpToDate = errors.New("git: branch already up to date with base")

// UpdateBranchOptions configures a "merge base into head" operation
// (the inverse of PerformMerge's direction). Used by F43/G8b's
// `pr update-branch` to advance the PR's head branch with the latest
// base content.
type UpdateBranchOptions struct {
	GitDir         string
	HeadRef        string // "refs/heads/<head-branch>"
	HeadOID        string // expected current tip of head
	BaseOID        string
	Method         string // "merge" | "rebase"
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	When           time.Time
	WorktreesDir   string
}

// UpdateBranchResult is the post-op state.
type UpdateBranchResult struct {
	NewHeadOID string
}

// UpdateBranchFromBase merges (or rebases) base into head, advancing
// the head_ref. Atomic update via update-ref CAS gated on the current
// head OID so a concurrent push aborts the operation cleanly.
//
// When head already contains base (the branch is current), returns
// ErrBranchAlreadyUpToDate without touching the ref.
//
// G8b (F43): pre-fix `shithub pr update-branch` 404'd against a verb
// the server never implemented — the CLI shipped with full client +
// tests against a vapor endpoint.
func UpdateBranchFromBase(ctx context.Context, opts UpdateBranchOptions) (UpdateBranchResult, error) {
	// Already up-to-date: base is an ancestor of head → nothing to do.
	if out, err := exec.CommandContext(ctx, "git", "-C", opts.GitDir,
		"merge-base", "--is-ancestor", opts.BaseOID, opts.HeadOID).CombinedOutput(); err == nil {
		return UpdateBranchResult{}, ErrBranchAlreadyUpToDate
	} else {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return UpdateBranchResult{}, fmt.Errorf("merge-base --is-ancestor: %w (%s)", err, out)
		}
		// Non-zero exit (typically 1) means "not an ancestor" — proceed.
	}

	if opts.WorktreesDir == "" {
		opts.WorktreesDir = filepath.Join(filepath.Dir(opts.GitDir), ".tmp-worktrees")
	}
	if err := os.MkdirAll(opts.WorktreesDir, 0o750); err != nil {
		return UpdateBranchResult{}, fmt.Errorf("worktrees dir: %w", err)
	}
	wt, err := os.MkdirTemp(opts.WorktreesDir, "updbr-*")
	if err != nil {
		return UpdateBranchResult{}, fmt.Errorf("mktemp worktree: %w", err)
	}
	defer func() {
		_ = exec.Command("git", "-C", opts.GitDir, "worktree", "remove", "--force", wt).Run()
		_ = os.RemoveAll(wt)
	}()

	// Worktree at head_oid (detached). We'll merge/rebase base into
	// it, then push the resulting tip to head_ref.
	if out, err := exec.CommandContext(ctx, "git", "-C", opts.GitDir,
		"worktree", "add", "--detach", wt, opts.HeadOID).CombinedOutput(); err != nil {
		return UpdateBranchResult{}, fmt.Errorf("worktree add: %w (%s)", err, out)
	}

	envBase := append(
		os.Environ(),
		"GIT_AUTHOR_NAME="+opts.AuthorName,
		"GIT_AUTHOR_EMAIL="+opts.AuthorEmail,
		"GIT_COMMITTER_NAME="+opts.CommitterName,
		"GIT_COMMITTER_EMAIL="+opts.CommitterEmail,
	)
	if !opts.When.IsZero() {
		envBase = append(
			envBase,
			"GIT_AUTHOR_DATE="+opts.When.Format(time.RFC3339),
			"GIT_COMMITTER_DATE="+opts.When.Format(time.RFC3339),
		)
	}

	switch opts.Method {
	case "", "merge":
		msg := "Merge base into " + strings.TrimPrefix(opts.HeadRef, "refs/heads/")
		mergeCmd := exec.CommandContext(ctx, "git", "-C", wt,
			"merge", "--no-ff", "--no-edit", "-m", msg, opts.BaseOID)
		mergeCmd.Env = envBase
		if out, err := mergeCmd.CombinedOutput(); err != nil {
			return UpdateBranchResult{}, fmt.Errorf("merge base into head: %w (%s)", err, out)
		}
	case "rebase":
		// Replay head's commits onto base_oid.
		rebaseCmd := exec.CommandContext(ctx, "git", "-C", wt,
			"rebase", "--onto", opts.BaseOID, opts.BaseOID, opts.HeadOID)
		rebaseCmd.Env = envBase
		if out, err := rebaseCmd.CombinedOutput(); err != nil {
			_ = exec.Command("git", "-C", wt, "rebase", "--abort").Run()
			return UpdateBranchResult{}, fmt.Errorf("rebase head onto base: %w (%s)", err, out)
		}
	default:
		return UpdateBranchResult{}, fmt.Errorf("unknown update-branch method %q", opts.Method)
	}

	revOut, err := exec.CommandContext(ctx, "git", "-C", wt, "rev-parse", "HEAD").Output()
	if err != nil {
		return UpdateBranchResult{}, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	newOID := strings.TrimSpace(string(revOut))

	// Atomic update-ref CAS on the head ref.
	if out, err := exec.CommandContext(ctx, "git", "-C", opts.GitDir,
		"update-ref", opts.HeadRef, newOID, opts.HeadOID).CombinedOutput(); err != nil {
		return UpdateBranchResult{}, fmt.Errorf("update-ref %s: %w (%s)", opts.HeadRef, err, out)
	}
	return UpdateBranchResult{NewHeadOID: newOID}, nil
}
