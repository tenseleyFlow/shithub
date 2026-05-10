// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"testing"

	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
)

func TestChooseFetchedDefaultBranch(t *testing.T) {
	t.Parallel()
	branches := []repogit.RefEntry{
		{Name: "feature", OID: "1111111111111111111111111111111111111111"},
		{Name: "main", OID: "2222222222222222222222222222222222222222"},
		{Name: "trunk", OID: "3333333333333333333333333333333333333333"},
	}
	name, oid := chooseFetchedDefaultBranch("trunk", branches)
	if name != "trunk" || oid != branches[2].OID {
		t.Fatalf("current default = %s %s, want trunk %s", name, oid, branches[2].OID)
	}

	name, oid = chooseFetchedDefaultBranch("missing", branches)
	if name != "trunk" || oid != branches[2].OID {
		t.Fatalf("fallback default = %s %s, want trunk %s", name, oid, branches[2].OID)
	}

	name, oid = chooseFetchedDefaultBranch("", []repogit.RefEntry{{Name: "zeta", OID: "aaaa"}})
	if name != "zeta" || oid != "aaaa" {
		t.Fatalf("first default = %s %s, want zeta aaaa", name, oid)
	}
}
