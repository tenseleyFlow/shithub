// SPDX-License-Identifier: AGPL-3.0-or-later

package git

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func TestParseGitmodules_ArmfortasStyle(t *testing.T) {
	t.Parallel()
	body := []byte(`[submodule "afs-as"]
	path = afs-as
	url = git@github.com:FortranGoingOnForty/afs-as.git
[submodule "bencch"]
	path = bencch
	url = https://github.com/tenseleyFlow/bencch.git
	branch = trunk
[submodule "afs-ld"]
	path = afs-ld
	url = git@github.com:FortranGoingOnForty/afs-ld.git
`)

	got := ParseGitmodules(body)
	if len(got) != 3 {
		t.Fatalf("len(ParseGitmodules) = %d, want 3", len(got))
	}
	if got["afs-as"].URL != "git@github.com:FortranGoingOnForty/afs-as.git" {
		t.Fatalf("afs-as URL = %q", got["afs-as"].URL)
	}
	if got["bencch"].Branch != "trunk" {
		t.Fatalf("bencch branch = %q, want trunk", got["bencch"].Branch)
	}
	if got["afs-ld"].Name != "afs-ld" {
		t.Fatalf("afs-ld name = %q, want afs-ld", got["afs-ld"].Name)
	}
}

func TestParseGitmodules_NormalizesQuotedPaths(t *testing.T) {
	t.Parallel()
	body := []byte(`[submodule "vendor/lib"]
	path = "./vendor/lib"
	url = "https://github.com/octo/lib.git"

[remote "origin"]
	url = ignored
`)

	got := ParseGitmodules(body)
	if _, ok := got["./vendor/lib"]; ok {
		t.Fatal("unexpected unclean path key")
	}
	sm, ok := got["vendor/lib"]
	if !ok {
		t.Fatalf("missing normalized vendor/lib key: %#v", got)
	}
	if sm.URL != "https://github.com/octo/lib.git" {
		t.Fatalf("URL = %q", sm.URL)
	}
}

func TestParseGitmodules_RejectsEscapingPath(t *testing.T) {
	t.Parallel()
	body := []byte(`[submodule "escape"]
path = ../escape
	url = https://github.com/octo/escape.git
`)

	got := ParseGitmodules(body)
	if len(got) != 0 {
		t.Fatalf("ParseGitmodules = %#v, want empty", got)
	}
}

func TestSubmodules_ReadsGitmodulesFromRef(t *testing.T) {
	t.Parallel()
	gitDir := t.TempDir()
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=trunk", gitDir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	build := InitialCommit{
		GitDir:      gitDir,
		AuthorName:  "Test Author",
		AuthorEmail: "test@example.com",
		Branch:      "trunk",
		When:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Files: []FileEntry{
			{
				Path: ".gitmodules",
				Body: []byte(`[submodule "bencch"]
	path = bencch
	url = https://github.com/tenseleyFlow/bencch.git
	branch = trunk
`),
			},
		},
	}
	if _, err := build.Build(context.Background()); err != nil {
		t.Fatalf("InitialCommit.Build: %v", err)
	}

	got, err := Submodules(context.Background(), gitDir, "trunk")
	if err != nil {
		t.Fatalf("Submodules: %v", err)
	}
	sm, ok := got["bencch"]
	if !ok {
		t.Fatalf("missing bencch submodule: %#v", got)
	}
	if sm.URL != "https://github.com/tenseleyFlow/bencch.git" {
		t.Fatalf("URL = %q", sm.URL)
	}
}
