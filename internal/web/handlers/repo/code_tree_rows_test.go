// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"testing"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestTreeEntryURL_EscapesPathSegments(t *testing.T) {
	t.Parallel()
	got := treeEntryURL("octo-user", "demo.repo", "feature/x", repogit.EntryBlob, "dir/a file.go")
	want := "/octo-user/demo.repo/blob/feature/x/dir/a%20file.go"
	if got != want {
		t.Fatalf("treeEntryURL = %q, want %q", got, want)
	}
}

func TestTreeEntryURL_NonNavigableEntries(t *testing.T) {
	t.Parallel()
	got := treeEntryURL("octo-user", "demo", "trunk", repogit.EntrySubmod, "vendor/lib")
	if got != "" {
		t.Fatalf("submodule URL = %q, want empty", got)
	}
}
