// SPDX-License-Identifier: AGPL-3.0-or-later

package finder_test

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/repos/finder"
)

func TestFilter_Empty(t *testing.T) {
	t.Parallel()
	paths := []string{"a", "b", "c"}
	got := finder.Filter(paths, "", 100)
	if len(got) != 3 {
		t.Errorf("len = %d, want 3", len(got))
	}
}

func TestFilter_PrefersBoundaryAndConsecutive(t *testing.T) {
	t.Parallel()
	paths := []string{
		"internal/web/handlers/repo/repo.go",
		"internal/repos/git/treeops.go",
		"docs/internal/repo-lifecycle.md",
		"internal/repos/lifecycle/rename.go",
	}
	got := finder.Filter(paths, "rename", 5)
	if len(got) == 0 {
		t.Fatalf("no matches")
	}
	if got[0].Path != "internal/repos/lifecycle/rename.go" {
		t.Errorf("top match = %q, want rename.go", got[0].Path)
	}
}

func TestFilter_Subsequence(t *testing.T) {
	t.Parallel()
	paths := []string{"main.go", "foo/bar/Main.go", "manifest.json"}
	got := finder.Filter(paths, "main", 5)
	if len(got) < 2 {
		t.Fatalf("got=%d, want at least 2", len(got))
	}
	// Both main.go variants should match; manifest.json doesn't (no
	// 'in' subsequence after 'a' break — actually m-a-n is there, but
	// the boundary score should prefer main.go).
	want := map[string]bool{"main.go": false, "foo/bar/Main.go": false}
	for _, m := range got {
		if _, ok := want[m.Path]; ok {
			want[m.Path] = true
		}
	}
	for p, seen := range want {
		if !seen {
			t.Errorf("expected %q in matches", p)
		}
	}
}

func TestFilter_NoMatchesEmpty(t *testing.T) {
	t.Parallel()
	got := finder.Filter([]string{"abc", "def"}, "xyz", 5)
	if len(got) != 0 {
		t.Errorf("unexpected matches: %+v", got)
	}
}

func TestFilter_LimitRespected(t *testing.T) {
	t.Parallel()
	paths := []string{"a.go", "ab.go", "abc.go", "abcd.go", "abcde.go"}
	got := finder.Filter(paths, "a", 3)
	if len(got) != 3 {
		t.Errorf("len=%d, want 3", len(got))
	}
}
