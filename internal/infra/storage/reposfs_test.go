// SPDX-License-Identifier: AGPL-3.0-or-later

package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mustNewRepoFS(t *testing.T) (*RepoFS, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := NewRepoFS(dir)
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	return r, dir
}

func TestNewRepoFS_RejectsRelativeRoot(t *testing.T) {
	t.Parallel()
	if _, err := NewRepoFS("relative/path"); err == nil {
		t.Fatal("expected error for relative root")
	}
}

func TestNewRepoFS_RejectsMissingRoot(t *testing.T) {
	t.Parallel()
	if _, err := NewRepoFS("/this/path/should/not/exist/abc123xyz"); err == nil {
		t.Fatal("expected error for missing root")
	}
}

func TestRepoPath_HappyPath(t *testing.T) {
	t.Parallel()
	r, root := mustNewRepoFS(t)
	got, err := r.RepoPath("alice", "my-project")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	want := filepath.Join(root, "al", "alice", "my-project.git")
	if got != want {
		t.Fatalf("RepoPath = %q, want %q", got, want)
	}
}

func TestRepoPath_AcceptsRepoExtraChars(t *testing.T) {
	t.Parallel()
	r, _ := mustNewRepoFS(t)
	for _, name := range []string{"name.with.dots", "name_under", "rust-by-example", "a1.b2_c3"} {
		if _, err := r.RepoPath("alice", name); err != nil {
			t.Errorf("RepoPath %q: %v", name, err)
		}
	}
}

func TestRepoPath_ShortOwnerPaddedShard(t *testing.T) {
	t.Parallel()
	r, root := mustNewRepoFS(t)
	got, err := r.RepoPath("a", "x")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	want := filepath.Join(root, "a_", "a", "x.git")
	if got != want {
		t.Fatalf("RepoPath = %q, want %q", got, want)
	}
}

func TestRepoPath_LowercasesOwner(t *testing.T) {
	t.Parallel()
	r, root := mustNewRepoFS(t)
	got, err := r.RepoPath("Alice", "Project")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	want := filepath.Join(root, "al", "alice", "project.git")
	if got != want {
		t.Fatalf("RepoPath = %q, want %q", got, want)
	}
}

// TestRepoPath_RejectsUnsafe is the critical path-validation table-driven
// test mandated by S04. Every entry MUST be rejected.
func TestRepoPath_RejectsUnsafe(t *testing.T) {
	t.Parallel()
	r, _ := mustNewRepoFS(t)

	cases := []struct {
		owner, name, why string
	}{
		{"", "name", "empty owner"},
		{"alice", "", "empty repo"},
		{"..", "name", "owner is .."},
		{"alice", "..", "repo is .."},
		{"al/ice", "name", "owner contains slash"},
		{"alice", "na/me", "repo contains slash"},
		{"alice", "../escape", "repo path traversal"},
		{"-leading", "name", "owner leading dash"},
		{"trailing-", "name", "owner trailing dash"},
		{"alice", "-leading", "repo leading dash"},
		{"alice", "trailing-", "repo trailing dash"},
		{".hidden", "name", "owner leading dot"},
		{"alice", ".hidden", "repo leading dot"},
		{"alice", ".git", "repo dotfile"},
		{"/absolute", "name", "owner absolute"},
		{"alice", "/absolute", "repo absolute"},
		{"alice", "name with space", "repo space"},
		{"alice", "name\x00null", "repo nul"},
		{"alice", "name\nnewline", "repo newline"},
		{"АliCe", "name", "owner non-ASCII (Cyrillic A)"},
		{"alice", "café", "repo non-ASCII"},
		{strings.Repeat("a", 40), "name", "owner too long"},
		{"alice", strings.Repeat("b", 101), "repo too long"},
		{"alice", "al!ice", "repo punctuation"},
		{"alice", "name@thing", "repo @"},
		{"al!ice", "name", "owner punctuation"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.why, func(t *testing.T) {
			t.Parallel()
			_, err := r.RepoPath(c.owner, c.name)
			if err == nil {
				t.Fatalf("expected error for %s (owner=%q, name=%q)", c.why, c.owner, c.name)
			}
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("expected ErrInvalidPath for %s, got %v", c.why, err)
			}
		})
	}
}

func TestExists_RejectsOutsideRoot(t *testing.T) {
	t.Parallel()
	r, _ := mustNewRepoFS(t)
	_, err := r.Exists("/etc/passwd")
	if !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("expected ErrEscapesRoot, got %v", err)
	}
}

func TestInitBare_HEADIsTrunk(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	r, _ := mustNewRepoFS(t)
	path, err := r.RepoPath("alice", "trunktest")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := r.InitBare(context.Background(), path); err != nil {
		t.Fatalf("InitBare: %v", err)
	}
	// G204: path comes from RepoPath in test setup (whitelisted).
	out, err := exec.Command("git", "--git-dir", path, "symbolic-ref", "HEAD").Output() //nolint:gosec
	if err != nil {
		t.Fatalf("symbolic-ref: %v", err)
	}
	got := strings.TrimSpace(string(out))
	if got != "refs/heads/trunk" {
		t.Fatalf("HEAD = %q, want refs/heads/trunk", got)
	}
}

func TestInitBare_RefusesNonEmpty(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	r, _ := mustNewRepoFS(t)
	path, err := r.RepoPath("alice", "twice")
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	if err := r.InitBare(context.Background(), path); err != nil {
		t.Fatalf("first InitBare: %v", err)
	}
	if err := r.InitBare(context.Background(), path); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists on second init, got %v", err)
	}
}

func TestMove_AtomicAndRefusesOverwrite(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not in PATH")
	}
	r, _ := mustNewRepoFS(t)
	src, _ := r.RepoPath("alice", "src")
	dst, _ := r.RepoPath("alice", "dst")
	if err := r.InitBare(context.Background(), src); err != nil {
		t.Fatalf("InitBare src: %v", err)
	}
	if err := r.Move(src, dst); err != nil {
		t.Fatalf("Move: %v", err)
	}
	srcExists, _ := r.Exists(src)
	dstExists, _ := r.Exists(dst)
	if srcExists || !dstExists {
		t.Fatalf("expected src absent and dst present, got src=%v dst=%v", srcExists, dstExists)
	}

	// Refuses overwrite: re-create src then attempt to move into existing dst.
	if err := r.InitBare(context.Background(), src); err != nil {
		t.Fatalf("re-init src: %v", err)
	}
	if err := r.Move(src, dst); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("expected ErrAlreadyExists, got %v", err)
	}
}

func TestDelete_RefusesEscape(t *testing.T) {
	t.Parallel()
	r, _ := mustNewRepoFS(t)
	if err := r.Delete("/etc/passwd"); !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("expected ErrEscapesRoot, got %v", err)
	}
}

func TestDelete_RefusesRoot(t *testing.T) {
	t.Parallel()
	r, root := mustNewRepoFS(t)
	if err := r.Delete(root); !errors.Is(err, ErrEscapesRoot) {
		t.Fatalf("expected ErrEscapesRoot for root, got %v", err)
	}
	// Root must still exist.
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root removed: %v", err)
	}
}
