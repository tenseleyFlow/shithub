// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
)

// commitStep is one commit in a fixture repo's history: a set of
// path→body writes (empty body means "delete"), plus its subject.
type commitStep struct {
	subject string
	write   map[string]string
	rename  [2]string
}

// buildHistoryRepo materializes a NON-bare repo with the given commit
// sequence and returns its .git dir. A worktree is the cheap way to
// script renames and deletions; every read helper under test takes a
// gitDir and doesn't care that an index exists next to it.
func buildHistoryRepo(t *testing.T, steps []commitStep) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=Test Author", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test Author", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "--initial-branch=trunk")
	for _, step := range steps {
		for p, body := range step.write {
			writeRepoFile(t, dir, p, body)
		}
		if step.rename[0] != "" {
			run("mv", step.rename[0], step.rename[1])
		}
		run("add", "-A")
		run("commit", "-q", "-m", step.subject)
	}
	return filepath.Join(dir, ".git")
}

func writeRepoFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func TestEntryLastCommits_ResolvesRootChildrenInOneWalk(t *testing.T) {
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1 seed", write: map[string]string{
			"README.md": "one\n", "src/a.go": "a\n", "docs/guide.md": "g\n",
		}},
		{subject: "c2 touch src", write: map[string]string{"src/a.go": "aa\n"}},
		{subject: "c3 touch readme", write: map[string]string{"README.md": "two\n"}},
	})

	before := gitops.ForkCount()
	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref:   "trunk",
		Names: []string{"README.md", "src", "docs"},
	})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	if forks := gitops.ForkCount() - before; forks != 1 {
		t.Errorf("fork count = %d, want 1", forks)
	}
	want := map[string]string{
		"README.md": "c3 touch readme",
		"src":       "c2 touch src",
		"docs":      "c1 seed",
	}
	for name, subject := range want {
		c, ok := got[name]
		if !ok {
			t.Errorf("%s unresolved", name)
			continue
		}
		if c.Subject != subject {
			t.Errorf("%s subject = %q, want %q", name, c.Subject, subject)
		}
		if len(c.OID) != 40 || c.ShortOID == "" || c.AuthorName != "Test Author" ||
			c.AuthorEmail != "test@example.com" || c.AuthorWhen.IsZero() {
			t.Errorf("%s: incomplete commit %+v", name, c)
		}
	}
}

func TestEntryLastCommits_NestedDirectoryAttributesDeepChanges(t *testing.T) {
	t.Parallel()
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1 seed", write: map[string]string{
			"src/pkg/deep/f.go": "1\n", "src/top.go": "1\n", "other/x": "1\n",
		}},
		{subject: "c2 deep", write: map[string]string{"src/pkg/deep/f.go": "2\n"}},
		{subject: "c3 unrelated", write: map[string]string{"other/x": "2\n"}},
	})

	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "trunk", Dir: "src", Names: []string{"pkg", "top.go"},
	})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	// A change three levels down still attributes to the immediate
	// child `pkg`, and the unrelated sibling commit is ignored.
	if got["pkg"].Subject != "c2 deep" {
		t.Errorf("pkg subject = %q, want %q", got["pkg"].Subject, "c2 deep")
	}
	if got["top.go"].Subject != "c1 seed" {
		t.Errorf("top.go subject = %q, want %q", got["top.go"].Subject, "c1 seed")
	}
}

func TestEntryLastCommits_RenameAttributesToRenameCommit(t *testing.T) {
	t.Parallel()
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1 seed", write: map[string]string{"old.txt": "x\n", "keep.txt": "k\n"}},
		{subject: "c2 rename", rename: [2]string{"old.txt", "new.txt"}},
	})

	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "trunk", Names: []string{"new.txt", "keep.txt"},
	})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	if got["new.txt"].Subject != "c2 rename" {
		t.Errorf("new.txt subject = %q, want %q", got["new.txt"].Subject, "c2 rename")
	}
	if got["keep.txt"].Subject != "c1 seed" {
		t.Errorf("keep.txt subject = %q, want %q", got["keep.txt"].Subject, "c1 seed")
	}
	// The pre-rename name no longer exists in the tree, so callers
	// never ask for it — and the walk must not invent an answer.
	if _, ok := got["old.txt"]; ok {
		t.Errorf("old.txt should not resolve; it is not a listed entry")
	}
}

func TestEntryLastCommits_UntouchedNameStaysUnresolved(t *testing.T) {
	t.Parallel()
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1 seed", write: map[string]string{"a.txt": "a\n"}},
	})

	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "trunk", Names: []string{"a.txt", "ghost.txt"},
	})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	if _, ok := got["a.txt"]; !ok {
		t.Errorf("a.txt should resolve")
	}
	if _, ok := got["ghost.txt"]; ok {
		t.Errorf("ghost.txt resolved but was never committed")
	}
}

func TestEntryLastCommits_WalkBoundLeavesStragglersUnresolved(t *testing.T) {
	t.Parallel()
	// `old.txt` is touched only by the very first commit; five newer
	// commits touch `hot.txt`. A bound of 2 cannot reach far enough
	// back, so `old.txt` must come back unresolved for the caller's
	// per-path fallback.
	steps := []commitStep{
		{subject: "c0 seed", write: map[string]string{"old.txt": "o\n", "hot.txt": "0\n"}},
	}
	for i := 1; i <= 5; i++ {
		steps = append(steps, commitStep{
			subject: "hot " + strconv.Itoa(i),
			write:   map[string]string{"hot.txt": strconv.Itoa(i) + "\n"},
		})
	}
	gitDir := buildHistoryRepo(t, steps)

	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "trunk", Names: []string{"hot.txt", "old.txt"}, MaxCommits: 2,
	})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	if got["hot.txt"].Subject != "hot 5" {
		t.Errorf("hot.txt subject = %q, want %q", got["hot.txt"].Subject, "hot 5")
	}
	if _, ok := got["old.txt"]; ok {
		t.Errorf("old.txt resolved despite a 2-commit walk bound")
	}

	// Without the bound the same walk resolves both.
	all, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "trunk", Names: []string{"hot.txt", "old.txt"},
	})
	if err != nil {
		t.Fatalf("EntryLastCommits (unbounded): %v", err)
	}
	if all["old.txt"].Subject != "c0 seed" {
		t.Errorf("old.txt subject = %q, want %q", all["old.txt"].Subject, "c0 seed")
	}
}

func TestEntryLastCommits_MatchesPerPathLogForEveryEntry(t *testing.T) {
	t.Parallel()
	// Parity harness: the single walk must agree with the N-fork
	// `git log -1 -- <path>` it replaces, entry for entry.
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1 seed", write: map[string]string{
			"README.md": "1\n", "src/a.go": "1\n", "src/b.go": "1\n",
			"docs/d1.md": "1\n", "vendor/lib/v.go": "1\n",
		}},
		{subject: "c2 src", write: map[string]string{"src/b.go": "2\n"}},
		{subject: "c3 vendor", write: map[string]string{"vendor/lib/v.go": "2\n"}},
		{subject: "c4 docs", write: map[string]string{"docs/d1.md": "2\n"}},
		{subject: "c5 readme", write: map[string]string{"README.md": "2\n"}},
	})

	names := []string{"README.md", "src", "docs", "vendor"}
	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "trunk", Names: names,
	})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	for _, name := range names {
		commits, err := gitops.Log(context.Background(), gitDir, gitops.LogOptions{
			Ref: "trunk", MaxCount: 1, Path: name,
		})
		if err != nil {
			t.Fatalf("Log %s: %v", name, err)
		}
		if len(commits) != 1 {
			t.Fatalf("Log %s returned %d commits", name, len(commits))
		}
		if got[name].OID != commits[0].OID {
			t.Errorf("%s: walk OID %q, per-path OID %q", name, got[name].OID, commits[0].OID)
		}
		if got[name].Subject != commits[0].Subject {
			t.Errorf("%s: walk subject %q, per-path subject %q", name, got[name].Subject, commits[0].Subject)
		}
	}
}

func TestEntryLastCommits_ForkCountIsConstantInEntryCount(t *testing.T) {
	// The point of the whole exercise: 1 fork for 5 entries, 1 fork
	// for 60. The old code shape was one `git log -1` per entry.
	for _, n := range []int{5, 60} {
		n := n
		t.Run(fmt.Sprintf("entries=%d", n), func(t *testing.T) {
			seed := map[string]string{}
			names := make([]string, 0, n)
			for i := 0; i < n; i++ {
				name := fmt.Sprintf("f%03d.txt", i)
				seed[name] = "x\n"
				names = append(names, name)
			}
			gitDir := buildHistoryRepo(t, []commitStep{{subject: "seed", write: seed}})

			before := gitops.ForkCount()
			got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
				Ref: "trunk", Names: names,
			})
			if err != nil {
				t.Fatalf("EntryLastCommits: %v", err)
			}
			if forks := gitops.ForkCount() - before; forks != 1 {
				t.Errorf("fork count for %d entries = %d, want 1", n, forks)
			}
			if len(got) != n {
				t.Errorf("resolved %d of %d entries", len(got), n)
			}
		})
	}
}

func TestEntryLastCommits_NoNamesIsANoop(t *testing.T) {
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1", write: map[string]string{"a.txt": "a\n"}},
	})
	before := gitops.ForkCount()
	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{Ref: "trunk"})
	if err != nil {
		t.Fatalf("EntryLastCommits: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
	if forks := gitops.ForkCount() - before; forks != 0 {
		t.Errorf("fork count = %d, want 0 (no names to resolve)", forks)
	}
}

func TestEntryLastCommits_BadRefReturnsError(t *testing.T) {
	t.Parallel()
	gitDir := buildHistoryRepo(t, []commitStep{
		{subject: "c1", write: map[string]string{"a.txt": "a\n"}},
	})
	got, err := gitops.EntryLastCommits(context.Background(), gitDir, gitops.LastCommitOptions{
		Ref: "no-such-ref", Names: []string{"a.txt"},
	})
	if err == nil {
		t.Fatalf("expected an error for a missing ref")
	}
	if len(got) != 0 {
		t.Errorf("got %d entries on a failed walk, want 0", len(got))
	}
}
