// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	gitops "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestResolveRef_LongestPrefixWins(t *testing.T) {
	t.Parallel()
	refs := []string{"main", "feature/x", "release/v1.0/beta"}
	cases := []struct {
		segs     []string
		wantRef  string
		wantPath string
		wantOK   bool
	}{
		{[]string{"main"}, "main", "", true},
		{[]string{"main", "src", "f.go"}, "main", "src/f.go", true},
		{[]string{"feature", "x"}, "feature/x", "", true},
		{[]string{"feature", "x", "sub", "f.go"}, "feature/x", "sub/f.go", true},
		{[]string{"release", "v1.0", "beta"}, "release/v1.0/beta", "", true},
		{[]string{"release", "v1.0", "beta", "README.md"}, "release/v1.0/beta", "README.md", true},
		{[]string{"missing"}, "", "", false},
		{[]string{}, "", "", false},
	}
	for _, c := range cases {
		ref, path, ok := gitops.ResolveRef(refs, c.segs)
		if ok != c.wantOK || ref != c.wantRef || path != c.wantPath {
			t.Errorf("segs=%v: got (%q, %q, %v), want (%q, %q, %v)",
				c.segs, ref, path, ok, c.wantRef, c.wantPath, c.wantOK)
		}
	}
}

func TestResolveRef_HexShortcut(t *testing.T) {
	t.Parallel()
	refs := []string{"main"}
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	ref, path, ok := gitops.ResolveRef(refs, []string{sha, "src", "f.go"})
	if !ok || ref != sha || path != "src/f.go" {
		t.Errorf("sha shortcut: got (%q, %q, %v)", ref, path, ok)
	}
}

func TestResolveRef_HexLooksLikeBranch(t *testing.T) {
	t.Parallel()
	// A branch named like a 40-hex string would be unusual; the spec
	// says ref-lookup takes priority. Here we don't list it as a ref,
	// so the SHA shortcut wins.
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	ref, _, ok := gitops.ResolveRef([]string{"main"}, []string{sha})
	if !ok || ref != sha {
		t.Errorf("expected SHA shortcut, got %q", ref)
	}
	// When the ref list contains the same string, ref-lookup wins.
	ref, path, ok := gitops.ResolveRef([]string{"main", sha}, []string{sha, "x"})
	if !ok || ref != sha || path != "x" {
		t.Errorf("ref-lookup should win: got (%q, %q, %v)", ref, path, ok)
	}
}

func TestListBlobs_RecursiveSizes(t *testing.T) {
	t.Parallel()
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
			{Path: "README.md", Body: []byte("# demo\n")},
			{Path: "cmd/app/main.go", Body: []byte("package main\n")},
			{Path: "web/index.html", Body: []byte("<h1>x</h1>\n")},
		},
	}
	if _, err := build.Build(context.Background()); err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}

	blobs, err := gitops.ListBlobs(context.Background(), dir, "trunk")
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	got := map[string]int64{}
	for _, b := range blobs {
		got[b.Path] = b.Size
	}
	if got["cmd/app/main.go"] != int64(len("package main\n")) {
		t.Errorf("go blob size = %d", got["cmd/app/main.go"])
	}
	if got["web/index.html"] != int64(len("<h1>x</h1>\n")) {
		t.Errorf("html blob size = %d", got["web/index.html"])
	}
	if got["README.md"] != int64(len("# demo\n")) {
		t.Errorf("readme blob size = %d", got["README.md"])
	}
}
