// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"testing"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestCompareURLsEscapeBranchSegments(t *testing.T) {
	got := compareURL("tenseleyFlow", "shithub", "trunk", "feature/a b")
	want := "/tenseleyFlow/shithub/compare/trunk...feature/a%20b"
	if got != want {
		t.Fatalf("compareURL() = %q, want %q", got, want)
	}

	got = pullNewURL("tenseleyFlow", "shithub", "trunk", "feature/a b")
	want = "/tenseleyFlow/shithub/pulls/new?base=trunk&head=feature%2Fa+b"
	if got != want {
		t.Fatalf("pullNewURL() = %q, want %q", got, want)
	}
}

func TestBuildCompareMenusPreservesOtherSide(t *testing.T) {
	refs := repogit.RefListing{
		Branches: []repogit.RefEntry{
			{Name: "trunk"},
			{Name: "scratch"},
		},
		Tags: []repogit.RefEntry{{Name: "v1.0.0"}},
	}

	baseMenu, headMenu := buildCompareMenus("octo", "demo", "trunk", "trunk", "scratch", refs, compareMenuTargetCompare)
	if baseMenu.Branches[1].Href != "/octo/demo/compare/scratch...scratch" {
		t.Fatalf("base branch href = %q", baseMenu.Branches[1].Href)
	}
	if headMenu.Branches[0].Href != "/octo/demo/compare/trunk...trunk" {
		t.Fatalf("head branch href = %q", headMenu.Branches[0].Href)
	}
	if !baseMenu.Branches[0].IsDefault {
		t.Fatalf("default branch not marked")
	}

	_, pullHeadMenu := buildCompareMenus("octo", "demo", "trunk", "trunk", "scratch", refs, compareMenuTargetPullNew)
	if pullHeadMenu.Branches[1].Href != "/octo/demo/pulls/new?base=trunk&head=scratch" {
		t.Fatalf("pull new head href = %q", pullHeadMenu.Branches[1].Href)
	}
}

func TestCountPatchFiles(t *testing.T) {
	patch := []byte(`diff --git a/one.txt b/one.txt
index 1111111..2222222 100644
--- a/one.txt
+++ b/one.txt
@@ -1 +1 @@
-old
+new
diff --git a/two.txt b/two.txt
new file mode 100644
`)
	if got := countPatchFiles(patch); got != 2 {
		t.Fatalf("countPatchFiles() = %d, want 2", got)
	}
}
