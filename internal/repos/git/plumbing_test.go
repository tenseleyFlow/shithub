// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// gitCmd is a thin wrapper that owns the single G204 suppression for
// the whole test file. Every test call goes through here; the bare-repo
// path is always a t.TempDir, never user input.
func gitCmd(args ...string) *exec.Cmd {
	//nolint:gosec // G204 false positive: callers feed t.TempDir paths and fixed flags.
	return exec.Command("git", args...)
}

// initBare creates a bare repo at dir/<name>.git and returns the path.
// Tests that need a bare repo to operate against use this rather than
// reaching into the storage package (one less inter-package dependency
// in the test).
func initBare(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	gitDir := filepath.Join(root, "x.git")
	if out, err := gitCmd("init", "--bare", "--initial-branch=trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	return gitDir
}

func TestInitialCommit_BuildSingleCommitWithFiles(t *testing.T) {
	t.Parallel()
	gitDir := initBare(t)

	when := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	ic := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice Anderson",
		AuthorEmail: "alice@example.com",
		Message:     "Initial commit",
		Branch:      "trunk",
		When:        when,
		Files: []repogit.FileEntry{
			{Path: "README.md", Body: []byte("# foo\n\nhello\n")},
			{Path: "LICENSE", Body: []byte("MIT-ish\n")},
			{Path: ".gitignore", Body: []byte("*.tmp\n")},
		},
	}
	commit, err := ic.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(commit) != 40 {
		t.Fatalf("commit OID looks wrong: %q", commit)
	}

	// HEAD must now resolve to the commit via refs/heads/trunk.
	out, err := gitCmd("-C", gitDir, "rev-parse", "refs/heads/trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != commit {
		t.Fatalf("rev-parse %q != commit %q", got, commit)
	}

	// Single commit, no parent.
	out, err = gitCmd("-C", gitDir, "rev-list", "--count", "trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-list: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("rev-list count = %q, want 1", got)
	}

	// Tree contents are the three files at the right paths.
	out, err = gitCmd("-C", gitDir, "ls-tree", "--name-only", "trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	wantSet := map[string]bool{"README.md": true, "LICENSE": true, ".gitignore": true}
	for _, name := range strings.Split(got, "\n") {
		if !wantSet[name] {
			t.Errorf("unexpected entry %q in tree (got=%q)", name, got)
		}
		delete(wantSet, name)
	}
	if len(wantSet) != 0 {
		t.Errorf("missing tree entries: %v", wantSet)
	}

	// Author identity is what we passed in.
	out, err = gitCmd("-C", gitDir, "log", "-1", "--format=%an <%ae>", "trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("log: %v\n%s", err, out)
	}
	if want := "Alice Anderson <alice@example.com>"; strings.TrimSpace(string(out)) != want {
		t.Errorf("author = %q, want %q", strings.TrimSpace(string(out)), want)
	}
}

func TestCommitAt_ResolvesBranchesAndSHAs(t *testing.T) {
	t.Parallel()
	gitDir := initBare(t)

	when := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	commit, err := repogit.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Alice Anderson",
		AuthorEmail: "alice@example.com",
		Message:     "Initial commit",
		Branch:      "trunk",
		When:        when,
		Files:       []repogit.FileEntry{{Path: "README.md", Body: []byte("# foo\n")}},
	}.Build(context.Background())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	for _, rev := range []string{"trunk", commit} {
		rev := rev
		t.Run(rev, func(t *testing.T) {
			t.Parallel()
			got, found, err := repogit.CommitAt(context.Background(), gitDir, rev)
			if err != nil {
				t.Fatalf("CommitAt(%q): %v", rev, err)
			}
			if !found {
				t.Fatalf("CommitAt(%q) found = false", rev)
			}
			if got.OID != commit {
				t.Fatalf("CommitAt(%q).OID = %q, want %q", rev, got.OID, commit)
			}
		})
	}

	_, found, err := repogit.CommitAt(context.Background(), gitDir, strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CommitAt(missing): %v", err)
	}
	if found {
		t.Fatal("CommitAt(missing) found = true, want false")
	}
}

func TestInitialCommit_RejectsEmptyInputs(t *testing.T) {
	t.Parallel()
	for _, ic := range []repogit.InitialCommit{
		{},
		{GitDir: "/tmp"},
		{GitDir: "/tmp", AuthorName: "x"},
		{GitDir: "/tmp", AuthorName: "x", AuthorEmail: "y"},
	} {
		if _, err := ic.Build(context.Background()); err == nil {
			t.Errorf("expected error for incomplete InitialCommit %+v", ic)
		}
	}
}
