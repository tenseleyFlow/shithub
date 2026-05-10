// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/tenseleyFlow/shithub/internal/actions/trigger"
	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// initRepo creates a fresh bare repo with the supplied files committed
// onto trunk and returns the path + the head SHA.
func initRepo(t *testing.T, files []gitops.FileEntry) (gitDir, sha string) {
	t.Helper()
	gitDir = t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	commit := gitops.InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
		Branch:      "trunk",
		When:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Files:       files,
	}
	commitSHA, err := commit.Build(context.Background())
	if err != nil {
		t.Fatalf("commit.Build: %v", err)
	}
	return gitDir, commitSHA
}

func TestDiscover_FindsWorkflows(t *testing.T) {
	t.Parallel()
	gitDir, sha := initRepo(t, []gitops.FileEntry{
		{Path: "README.md", Body: []byte("# demo\n")},
		{Path: ".shithub/workflows/ci.yml", Body: []byte("name: ci\non: push\njobs:\n  j:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo\n")},
		{Path: ".shithub/workflows/release.yaml", Body: []byte("name: release\n")},
		{Path: ".shithub/workflows/README.md", Body: []byte("notes\n")}, // non-yaml ignored
	})
	files, skips, err := trigger.Discover(context.Background(), gitDir, sha)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(skips) != 0 {
		t.Errorf("unexpected skips: %v", skips)
	}
	got := map[string]bool{}
	for _, f := range files {
		got[f.Path] = true
	}
	if !got[".shithub/workflows/ci.yml"] || !got[".shithub/workflows/release.yaml"] {
		t.Errorf("missing expected workflow files; got %v", got)
	}
	if got[".shithub/workflows/README.md"] {
		t.Error("non-yaml files should be filtered out")
	}
}

func TestDiscover_NoWorkflowsDirIsClean(t *testing.T) {
	t.Parallel()
	gitDir, sha := initRepo(t, []gitops.FileEntry{
		{Path: "README.md", Body: []byte("# demo\n")},
	})
	files, skips, err := trigger.Discover(context.Background(), gitDir, sha)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(files) != 0 || len(skips) != 0 {
		t.Errorf("repo without .shithub/workflows must return clean empty; got files=%v skips=%v", files, skips)
	}
}

func TestDiscover_OversizedFileGoesToSkips(t *testing.T) {
	t.Parallel()
	// A 65 KB workflow exceeds workflow.MaxWorkflowFileBytes (64 KB).
	big := make([]byte, 65*1024)
	for i := range big {
		big[i] = ' '
	}
	gitDir, sha := initRepo(t, []gitops.FileEntry{
		{Path: ".shithub/workflows/huge.yml", Body: big},
		{Path: ".shithub/workflows/normal.yml", Body: []byte("name: ok\n")},
	})
	files, skips, err := trigger.Discover(context.Background(), gitDir, sha)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// huge.yml should appear in skips, normal.yml in files. Discovery
	// is per-file: one bad file doesn't tank the others on the commit.
	hasNormal := false
	for _, f := range files {
		if f.Path == ".shithub/workflows/normal.yml" {
			hasNormal = true
		}
	}
	if !hasNormal {
		t.Error("normal.yml must still be discovered when huge.yml is skipped")
	}
	hasHuge := false
	for _, s := range skips {
		if s.Path == ".shithub/workflows/huge.yml" {
			hasHuge = true
		}
	}
	if !hasHuge {
		t.Errorf("huge.yml must surface in skips; got %v", skips)
	}
}
