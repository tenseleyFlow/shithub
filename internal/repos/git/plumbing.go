// SPDX-License-Identifier: AGPL-3.0-or-later

// Package git wraps the git plumbing commands we need to build the
// optional initial commit on a freshly-init'd bare repo. Plumbing-only,
// no working tree: hash-object → update-index (with GIT_INDEX_FILE) →
// write-tree → commit-tree → update-ref. This is what `git` itself does
// internally; we drive it from outside via short-lived subprocesses so
// we don't need to vendor a Go-native git implementation.
//
// Every shell-out is constrained to a caller-supplied gitDir we control
// (`storage.RepoFS.RepoPath` produces it from a strict path whitelist).
// `git -C` is used instead of changing the process working directory.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// FileEntry is one file destined for the initial commit. Path is the
// repository-relative slash-separated path (no leading slash). Body is
// the raw bytes; we hash whatever we're given without normalization.
type FileEntry struct {
	Path string
	Body []byte
}

// InitialCommit describes the single commit produced for a freshly
// initialized repo whose owner ticked "initialize with README/license/
// .gitignore" on the create form. Everything except Files defaults to
// sensible values; callers must always set GitDir + Author* + Files.
type InitialCommit struct {
	GitDir      string // absolute bare-repo path (must already exist + be inited)
	AuthorName  string
	AuthorEmail string
	Message     string    // defaults to "Initial commit"
	Branch      string    // defaults to "trunk"
	When        time.Time // defaults to time.Now()
	Files       []FileEntry
}

// Build runs the full plumbing sequence and returns the new commit OID.
// On error the bare repo may have orphan blobs/trees written; the
// caller is expected to RemoveAll the bare repo dir on failure.
func (ic InitialCommit) Build(ctx context.Context) (string, error) {
	if ic.GitDir == "" {
		return "", errors.New("git plumbing: GitDir is required")
	}
	if ic.AuthorName == "" || ic.AuthorEmail == "" {
		return "", errors.New("git plumbing: author name/email are required")
	}
	if len(ic.Files) == 0 {
		return "", errors.New("git plumbing: at least one file is required")
	}
	if ic.Message == "" {
		ic.Message = "Initial commit"
	}
	if ic.Branch == "" {
		ic.Branch = "trunk"
	}
	if ic.When.IsZero() {
		ic.When = time.Now()
	}

	indexFile, err := os.CreateTemp("", "shithub-index-*")
	if err != nil {
		return "", fmt.Errorf("git plumbing: temp index: %w", err)
	}
	indexPath := indexFile.Name()
	_ = indexFile.Close()
	defer func() { _ = os.Remove(indexPath) }()
	// git refuses to use an empty file as an index — delete now and let
	// `update-index` recreate it on first use.
	_ = os.Remove(indexPath)

	for _, f := range ic.Files {
		oid, err := ic.hashObject(ctx, f.Body)
		if err != nil {
			return "", fmt.Errorf("hash-object %s: %w", f.Path, err)
		}
		if err := ic.updateIndex(ctx, indexPath, oid, f.Path); err != nil {
			return "", fmt.Errorf("update-index %s: %w", f.Path, err)
		}
	}

	tree, err := ic.writeTree(ctx, indexPath)
	if err != nil {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	commit, err := ic.commitTree(ctx, tree)
	if err != nil {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	if err := ic.updateRef(ctx, commit); err != nil {
		return "", fmt.Errorf("update-ref: %w", err)
	}
	return commit, nil
}

// hashObject pipes body through `git hash-object -w --stdin` and
// returns the resulting OID.
func (ic InitialCommit) hashObject(ctx context.Context, body []byte) (string, error) {
	//nolint:gosec // G204: gitDir is constrained by storage.RepoFS path validation.
	cmd := exec.CommandContext(ctx, "git", "-C", ic.GitDir, "hash-object", "-w", "--stdin")
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		return "", wrapExecErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// updateIndex stages a blob at path under the temp index pointed at by
// indexPath. Mode is fixed at 100644 (regular file); the spec doesn't
// emit symlinks or executables for the initial commit.
func (ic InitialCommit) updateIndex(ctx context.Context, indexPath, oid, path string) error {
	cacheinfo := fmt.Sprintf("100644,%s,%s", oid, path)
	//nolint:gosec // G204: gitDir + path are validated upstream.
	cmd := exec.CommandContext(ctx, "git", "-C", ic.GitDir, "update-index", "--add", "--cacheinfo", cacheinfo)
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", wrapExecErr(err), strings.TrimSpace(string(out)))
	}
	return nil
}

// writeTree turns the staged index into a tree object and returns its OID.
func (ic InitialCommit) writeTree(ctx context.Context, indexPath string) (string, error) {
	//nolint:gosec // G204: gitDir validated upstream.
	cmd := exec.CommandContext(ctx, "git", "-C", ic.GitDir, "write-tree")
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+indexPath)
	out, err := cmd.Output()
	if err != nil {
		return "", wrapExecErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// commitTree builds a commit object pointing at tree, with no parent.
// Author + committer are both populated from ic.Author*; ic.When fixes
// both timestamps so the test suite gets deterministic OIDs.
func (ic InitialCommit) commitTree(ctx context.Context, tree string) (string, error) {
	//nolint:gosec // G204: tree is git's stdout (40-char OID); gitDir validated.
	cmd := exec.CommandContext(ctx, "git", "-C", ic.GitDir, "commit-tree", tree, "-m", ic.Message)
	stamp := ic.When.Format(time.RFC3339)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+ic.AuthorName,
		"GIT_AUTHOR_EMAIL="+ic.AuthorEmail,
		"GIT_AUTHOR_DATE="+stamp,
		"GIT_COMMITTER_NAME="+ic.AuthorName,
		"GIT_COMMITTER_EMAIL="+ic.AuthorEmail,
		"GIT_COMMITTER_DATE="+stamp,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", wrapExecErr(err)
	}
	return strings.TrimSpace(string(out)), nil
}

// updateRef points refs/heads/<Branch> at commit. After this call the
// bare repo's HEAD (a symbolic ref to refs/heads/<Branch>) finally
// resolves to a real commit.
func (ic InitialCommit) updateRef(ctx context.Context, commit string) error {
	ref := "refs/heads/" + ic.Branch
	//nolint:gosec // G204: ref is constructed from a non-empty branch name we set.
	cmd := exec.CommandContext(ctx, "git", "-C", ic.GitDir, "update-ref", ref, commit)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", wrapExecErr(err), strings.TrimSpace(string(out)))
	}
	return nil
}

// wrapExecErr unwraps an *exec.ExitError to expose stderr in the
// returned message; on other errors it passes through. Useful when the
// caller logs %w errors and we want the actual git stderr in the line.
func wrapExecErr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		stderr := strings.TrimSpace(string(ee.Stderr))
		if stderr != "" {
			return fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return err
}
