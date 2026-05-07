// SPDX-License-Identifier: AGPL-3.0-or-later

package protocol_test

import (
	"errors"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/git/protocol"
)

func TestParseSSHCommand_Accepts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in           string
		wantService  protocol.Service
		wantOwner    string
		wantRepo     string
	}{
		{"git-upload-pack 'alice/foo'", protocol.UploadPack, "alice", "foo"},
		{"git-upload-pack 'alice/foo.git'", protocol.UploadPack, "alice", "foo"},
		{"git-receive-pack 'bob/bar'", protocol.ReceivePack, "bob", "bar"},
		{"git-receive-pack alice/foo.git", protocol.ReceivePack, "alice", "foo"},
		{"git-upload-pack 'alice/rust-by-example'", protocol.UploadPack, "alice", "rust-by-example"},
		{"git-upload-pack 'alice/name.with.dots'", protocol.UploadPack, "alice", "name.with.dots"},
	}
	for _, c := range cases {
		got, err := protocol.ParseSSHCommand(c.in)
		if err != nil {
			t.Errorf("%q: %v", c.in, err)
			continue
		}
		if got.Service != c.wantService || got.Owner != c.wantOwner || got.Repo != c.wantRepo {
			t.Errorf("%q: got %+v", c.in, got)
		}
	}
}

func TestParseSSHCommand_RejectsUnknown(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		" ",
		"ls",
		"bash",
		"git-archive 'alice/foo'",
		"git-upload-pack",                            // no path
		" git-upload-pack 'a/b'",                     // leading space
		"git-upload-pack 'a/b' ",                     // trailing space
		"git-upload-pack 'a/b' && rm -rf /",          // command injection
		`git-upload-pack 'a/b'; cat /etc/passwd`,
		"GIT-UPLOAD-PACK 'a/b'",                       // case
		"git-upload-pack a/b'",                        // mismatched quotes
	} {
		_, err := protocol.ParseSSHCommand(in)
		if !errors.Is(err, protocol.ErrUnknownSSHCommand) {
			t.Errorf("%q: err = %v, want ErrUnknownSSHCommand", in, err)
		}
	}
}

func TestParseSSHCommand_RejectsBadPath(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"git-upload-pack '../../etc/passwd'",
		"git-upload-pack 'alice/../bob/foo'",
		"git-upload-pack 'alice/foo/extra'",
		"git-upload-pack '/alice/foo'",
		"git-upload-pack 'alice/'",
		"git-upload-pack '/foo'",
		"git-upload-pack 'alice/.git-meta'",
		"git-upload-pack 'alice/-leading'",
		"git-upload-pack 'alice/trailing-'",
		"git-upload-pack 'al!ce/foo'",
		"git-upload-pack 'alice/foo bar'",
	} {
		_, err := protocol.ParseSSHCommand(in)
		if !errors.Is(err, protocol.ErrInvalidSSHPath) {
			t.Errorf("%q: err = %v, want ErrInvalidSSHPath", in, err)
		}
	}
}

func TestParseSSHCommand_StripsSingleDotGit(t *testing.T) {
	t.Parallel()
	got, err := protocol.ParseSSHCommand("git-upload-pack 'alice/foo.git'")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.Repo != "foo" {
		t.Errorf("Repo = %q, want %q", got.Repo, "foo")
	}
	// And nested .git.git would split to ".git" repo (which would then
	// be rejected because leading dot — but the parser doesn't do extra
	// stripping past the single suffix).
	got2, err := protocol.ParseSSHCommand("git-upload-pack 'alice/foo.git.git'")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got2.Repo != "foo.git" {
		t.Errorf("nested .git.git: Repo = %q, want %q", got2.Repo, "foo.git")
	}
}
