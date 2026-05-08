// SPDX-License-Identifier: AGPL-3.0-or-later

package git_test

import (
	"testing"

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
