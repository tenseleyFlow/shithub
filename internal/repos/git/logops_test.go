// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// TestLog_AndGetCommit_HappyPath drives the log + commit-detail
// helpers against a freshly-built bare repo. Verifies parsing of
// every field that the templates consume.
func TestLog_AndGetCommit_HappyPath(t *testing.T) {
	t.Parallel()
	gitDir := buildSeedRepo(t)

	commits, err := gitops.Log(context.Background(), gitDir, gitops.LogOptions{Ref: "trunk"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(commits) == 0 {
		t.Fatalf("Log returned 0 commits")
	}
	c := commits[0]
	if c.OID == "" || len(c.OID) != 40 {
		t.Errorf("OID = %q", c.OID)
	}
	if c.Subject == "" {
		t.Errorf("Subject empty")
	}
	if c.AuthorWhen.IsZero() {
		t.Errorf("AuthorWhen unset")
	}

	detail, err := gitops.GetCommit(context.Background(), gitDir, c.OID)
	if err != nil {
		t.Fatalf("GetCommit: %v", err)
	}
	if detail.OID != c.OID {
		t.Errorf("detail.OID = %q, want %q", detail.OID, c.OID)
	}
	if len(detail.Files) == 0 {
		t.Errorf("Files empty for initial commit")
	}
	for _, f := range detail.Files {
		if f.Status == "" || f.Path == "" {
			t.Errorf("malformed FileChange: %+v", f)
		}
	}
	if detail.TreeOID == "" {
		t.Errorf("TreeOID empty")
	}

	count, err := gitops.CountCommits(context.Background(), gitDir, "trunk")
	if err != nil {
		t.Fatalf("CountCommits: %v", err)
	}
	if count != 1 {
		t.Fatalf("CountCommits = %d, want 1", count)
	}
}

func TestGetCommit_ReturnsErrCommitNotFound(t *testing.T) {
	t.Parallel()
	gitDir := buildSeedRepo(t)
	_, err := gitops.GetCommit(context.Background(), gitDir, strings.Repeat("0", 40))
	if !errors.Is(err, gitops.ErrCommitNotFound) {
		t.Fatalf("GetCommit error = %v, want commit not found", err)
	}
}

func TestCommitExists(t *testing.T) {
	t.Parallel()
	gitDir := buildSeedRepo(t)
	commits, err := gitops.Log(context.Background(), gitDir, gitops.LogOptions{Ref: "trunk"})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	exists, err := gitops.CommitExists(context.Background(), gitDir, commits[0].OID)
	if err != nil {
		t.Fatalf("CommitExists(existing): %v", err)
	}
	if !exists {
		t.Fatal("CommitExists(existing) = false, want true")
	}
	exists, err = gitops.CommitExists(context.Background(), gitDir, strings.Repeat("0", 40))
	if err != nil {
		t.Fatalf("CommitExists(missing): %v", err)
	}
	if exists {
		t.Fatal("CommitExists(missing) = true, want false")
	}
}

func TestWeeklyCommitActivity_BucketsByUTCWeek(t *testing.T) {
	t.Parallel()
	gitDir := buildSeedRepo(t)
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

	buckets, err := gitops.WeeklyCommitActivity(context.Background(), gitDir, "trunk", 4, now)
	if err != nil {
		t.Fatalf("WeeklyCommitActivity: %v", err)
	}
	want := []int{0, 1, 0, 0}
	if len(buckets) != len(want) {
		t.Fatalf("bucket len = %d, want %d: %v", len(buckets), len(want), buckets)
	}
	for i := range want {
		if buckets[i] != want[i] {
			t.Fatalf("bucket[%d] = %d, want %d: %v", i, buckets[i], want[i], buckets)
		}
	}
}

func TestBlame_HappyPath(t *testing.T) {
	t.Parallel()
	gitDir := buildSeedRepo(t)
	chunks, err := gitops.Blame(context.Background(), gitDir, gitops.BlameOptions{
		Ref:  "trunk",
		Path: "README.md",
	})
	if err != nil {
		t.Fatalf("Blame: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatalf("blame returned no chunks")
	}
	for _, c := range chunks {
		if c.OID == "" || len(c.OID) != 40 {
			t.Errorf("chunk OID = %q", c.OID)
		}
		if c.AuthorName == "" {
			t.Errorf("chunk AuthorName empty")
		}
		if c.AuthorWhen.IsZero() {
			t.Errorf("chunk AuthorWhen unset")
		}
		if c.StartLine == 0 || c.EndLine == 0 || c.StartLine > c.EndLine {
			t.Errorf("chunk lines: start=%d end=%d", c.StartLine, c.EndLine)
		}
	}
}

// buildSeedRepo materializes a bare repo with one templated initial
// commit (README.md) on the trunk branch. Reuses InitialCommit's
// plumbing — same path the production create-repo flow exercises.
func buildSeedRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=trunk", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	build := gitops.InitialCommit{
		GitDir:      dir,
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
		Branch:      "trunk",
		When:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Files: []gitops.FileEntry{
			{Path: "README.md", Body: []byte("# demo\n\nHello.\n")},
		},
	}
	if _, err := build.Build(context.Background()); err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}
	return dir
}
