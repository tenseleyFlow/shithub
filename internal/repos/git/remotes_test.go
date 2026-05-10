// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"context"
	"strings"
	"testing"
	"time"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestFetchRemoteHeadsAndTags_ImportsReachableCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initBare(t)
	commit, err := repogit.InitialCommit{
		GitDir:      source,
		AuthorName:  "Alice Anderson",
		AuthorEmail: "alice@example.com",
		Message:     "source commit",
		Branch:      "trunk",
		When:        time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		Files:       []repogit.FileEntry{{Path: "README.md", Body: []byte("# source\n")}},
	}.Build(ctx)
	if err != nil {
		t.Fatalf("Build source: %v", err)
	}
	dst := initBare(t)

	if err := repogit.FetchRemoteHeadsAndTags(ctx, dst, source); err != nil {
		t.Fatalf("FetchRemoteHeadsAndTags: %v", err)
	}
	exists, err := repogit.CommitExists(ctx, dst, commit)
	if err != nil {
		t.Fatalf("CommitExists: %v", err)
	}
	if !exists {
		t.Fatalf("fetched repo is missing commit %s", commit)
	}
	out, err := gitCmd("-C", dst, "rev-parse", "refs/heads/trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse dst trunk: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != commit {
		t.Fatalf("dst trunk = %q, want %q", got, commit)
	}
}

func TestFetchRemoteHeadsAndTags_DoesNotForceDivergedBranch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	source := initBare(t)
	if _, err := (repogit.InitialCommit{
		GitDir:      source,
		AuthorName:  "Alice Anderson",
		AuthorEmail: "alice@example.com",
		Message:     "source commit",
		Branch:      "trunk",
		When:        time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC),
		Files:       []repogit.FileEntry{{Path: "README.md", Body: []byte("# source\n")}},
	}).Build(ctx); err != nil {
		t.Fatalf("Build source: %v", err)
	}
	dst := initBare(t)
	dstCommit, err := repogit.InitialCommit{
		GitDir:      dst,
		AuthorName:  "Bob Brown",
		AuthorEmail: "bob@example.com",
		Message:     "local commit",
		Branch:      "trunk",
		When:        time.Date(2026, 5, 10, 12, 1, 0, 0, time.UTC),
		Files:       []repogit.FileEntry{{Path: "README.md", Body: []byte("# local\n")}},
	}.Build(ctx)
	if err != nil {
		t.Fatalf("Build dst: %v", err)
	}

	if err := repogit.FetchRemoteHeadsAndTags(ctx, dst, source); err == nil {
		t.Fatal("FetchRemoteHeadsAndTags succeeded on a diverged branch; want rejection")
	}
	out, err := gitCmd("-C", dst, "rev-parse", "refs/heads/trunk").CombinedOutput()
	if err != nil {
		t.Fatalf("rev-parse dst trunk: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != dstCommit {
		t.Fatalf("dst trunk changed to %q, want original %q", got, dstCommit)
	}
}
