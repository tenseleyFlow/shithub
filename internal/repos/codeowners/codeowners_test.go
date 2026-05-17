// SPDX-License-Identifier: AGPL-3.0-or-later

package codeowners

import "testing"

func TestParseOwnersAndLastMatchWins(t *testing.T) {
	t.Parallel()
	f := Parse("CODEOWNERS", []byte(`
# comments are ignored
* @global
*.go @go-owner @acme/go-reviewers
/cmd/ @cli
/cmd/shithubd/main.go @specific
/cmd/shithubd/generated.go
`))
	if len(f.Errors) != 0 {
		t.Fatalf("unexpected parse errors: %+v", f.Errors)
	}

	entry, ok := f.OwnersFor("cmd/shithubd/main.go")
	if !ok {
		t.Fatal("expected match")
	}
	if entry.Pattern != "/cmd/shithubd/main.go" {
		t.Fatalf("pattern=%q", entry.Pattern)
	}
	if got := owners(entry); got != "user:specific" {
		t.Fatalf("owners=%s", got)
	}

	entry, ok = f.OwnersFor("internal/pulls/pulls.go")
	if !ok {
		t.Fatal("expected match")
	}
	if entry.Pattern != "*.go" {
		t.Fatalf("pattern=%q", entry.Pattern)
	}
	if got := owners(entry); got != "user:go-owner,team:acme/go-reviewers" {
		t.Fatalf("owners=%s", got)
	}

	entry, ok = f.OwnersFor("cmd/shithubd/generated.go")
	if !ok {
		t.Fatal("expected match")
	}
	if entry.Pattern != "/cmd/shithubd/generated.go" {
		t.Fatalf("pattern=%q", entry.Pattern)
	}
	if len(entry.Owners) != 0 {
		t.Fatalf("empty-owner override should clear owners, got %+v", entry.Owners)
	}
}

func TestParseSkipsUnsupportedPatterns(t *testing.T) {
	t.Parallel()
	f := Parse("CODEOWNERS", []byte(`
!negated @nope
[a-z].go @nope
\#literal @nope
src/*.go not-an-owner
src/*.txt @docs
`))
	if len(f.Errors) < 4 {
		t.Fatalf("expected unsupported syntax errors, got %+v", f.Errors)
	}
	if _, ok := f.OwnersFor("negated"); ok {
		t.Fatal("negation line should be skipped")
	}
	if _, ok := f.OwnersFor("a.go"); ok {
		t.Fatal("range line should be skipped")
	}
	if _, ok := f.OwnersFor("#literal"); ok {
		t.Fatal("escaped comment pattern should be skipped")
	}
	if _, ok := f.OwnersFor("src/main.go"); ok {
		t.Fatal("line with only invalid owners should be skipped")
	}
	entry, ok := f.OwnersFor("src/readme.txt")
	if !ok {
		t.Fatal("valid line should match")
	}
	if got := owners(entry); got != "user:docs" {
		t.Fatalf("owners=%s", got)
	}
}

func TestParseEscapedSpacesAndInlineComments(t *testing.T) {
	t.Parallel()
	f := Parse("CODEOWNERS", []byte(`
docs/My\ File.md @docs # inline comment
`))
	if len(f.Errors) != 0 {
		t.Fatalf("unexpected parse errors: %+v", f.Errors)
	}
	entry, ok := f.OwnersFor("docs/My File.md")
	if !ok {
		t.Fatal("expected escaped-space pattern to match")
	}
	if got := owners(entry); got != "user:docs" {
		t.Fatalf("owners=%s", got)
	}
}

func TestDirectoryPatternMatchesDescendants(t *testing.T) {
	t.Parallel()
	f := Parse("CODEOWNERS", []byte(`
docs/ @docs
`))
	if _, ok := f.OwnersFor("docs"); !ok {
		t.Fatal("directory pattern should match directory path")
	}
	if _, ok := f.OwnersFor("docs/readme.md"); !ok {
		t.Fatal("directory pattern should match descendants")
	}
	if _, ok := f.OwnersFor("src/docs/readme.md"); !ok {
		t.Fatal("unanchored directory pattern should match nested directories")
	}
}

func owners(entry Entry) string {
	out := ""
	for i, owner := range entry.Owners {
		if i > 0 {
			out += ","
		}
		switch owner.Kind {
		case OwnerTeam:
			out += "team:" + owner.Org + "/" + owner.Team
		case OwnerUser:
			out += "user:" + owner.Username
		case OwnerEmail:
			out += "email:" + owner.Email
		}
	}
	return out
}
