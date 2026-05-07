// SPDX-License-Identifier: AGPL-3.0-or-later

package passwords

import "testing"

func TestIsCommon(t *testing.T) {
	t.Parallel()
	common := []string{"password", "123456", "qwerty", "letmein", "iloveyou"}
	for _, p := range common {
		if !IsCommon(p) {
			t.Errorf("expected %q to be flagged as common", p)
		}
	}
}

func TestIsCommon_CaseInsensitive(t *testing.T) {
	t.Parallel()
	if !IsCommon("Password") || !IsCommon("PASSWORD") {
		t.Fatal("common-password match should be case-insensitive")
	}
}

func TestIsCommon_AllowsStrong(t *testing.T) {
	t.Parallel()
	strong := "Tr0ub4dor-correct-horse-battery-staple"
	if IsCommon(strong) {
		t.Fatalf("strong password %q wrongly flagged as common", strong)
	}
}

func TestSize_AtLeast9000(t *testing.T) {
	t.Parallel()
	// We embed SecLists' 10k-most-common (some duplicates collapse on
	// case-fold). Allow some slack but require the list to actually load.
	if got := Size(); got < 9000 {
		t.Fatalf("Size() = %d; expected at least 9000 entries", got)
	}
}
