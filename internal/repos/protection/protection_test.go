// SPDX-License-Identifier: AGPL-3.0-or-later

package protection

import (
	"testing"

	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func TestMatchRule_LongestPrefixWins(t *testing.T) {
	t.Parallel()
	rules := []reposdb.BranchProtectionRule{
		{ID: 1, Pattern: "*"},
		{ID: 2, Pattern: "release/*"},
		{ID: 3, Pattern: "release/v1.0"},
	}
	cases := []struct {
		branch  string
		wantID  int64
		wantHit bool
	}{
		{"release/v1.0", 3, true},
		{"release/beta", 2, true},
		// `*` doesn't cross `/` per filepath.Match — no rule matches
		// a slash-containing branch when no rule has a deeper pattern.
		{"release/v1.0/sub", 0, false},
		{"trunk", 1, true},
		{"feature-x", 1, true},
	}
	for _, c := range cases {
		got, ok := matchRule(rules, c.branch)
		if ok != c.wantHit {
			t.Errorf("branch %q: hit=%v want %v", c.branch, ok, c.wantHit)
			continue
		}
		if !c.wantHit {
			continue
		}
		if got.ID != c.wantID {
			t.Errorf("branch %q: matched rule %d, want %d", c.branch, got.ID, c.wantID)
		}
	}
}

func TestMatchRule_NoMatchReturnsFalse(t *testing.T) {
	t.Parallel()
	rules := []reposdb.BranchProtectionRule{{Pattern: "release/*"}}
	if _, ok := matchRule(rules, "trunk"); ok {
		t.Errorf("expected no match for trunk against release/*")
	}
}

func TestMatchRule_AlphabeticalTiebreak(t *testing.T) {
	t.Parallel()
	rules := []reposdb.BranchProtectionRule{
		{ID: 1, Pattern: "feature/*"},
		{ID: 2, Pattern: "feat[u]re/*"}, // same length matching same string
	}
	got, ok := matchRule(rules, "feature/foo")
	if !ok {
		t.Fatalf("expected match")
	}
	// Both patterns have same length; alphabetical tiebreak:
	// "feat[u]re/*" < "feature/*" so the [u]re/* wins.
	if got.ID != 2 {
		t.Errorf("got rule %d, want 2 (alphabetical tiebreak)", got.ID)
	}
}

func TestMatchRule_TargetIsolation(t *testing.T) {
	t.Parallel()
	rules := []reposdb.BranchProtectionRule{
		{ID: 1, Pattern: "v*", Target: "tag"},
		{ID: 2, Pattern: "v*", Target: "branch"},
		{ID: 3, Pattern: "release/*", Target: "tag"},
	}
	got, ok := matchRule(rules, "v1.0.0")
	if !ok {
		t.Fatalf("expected branch match")
	}
	if got.ID != 2 {
		t.Fatalf("branch match id=%d want 2", got.ID)
	}
	got, ok = matchRuleForTarget(rules, "tag", "v1.0.0")
	if !ok {
		t.Fatalf("expected tag match")
	}
	if got.ID != 1 {
		t.Fatalf("tag match id=%d want 1", got.ID)
	}
}

func TestTargetAndNameForRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		ref        string
		wantTarget string
		wantName   string
		wantOK     bool
	}{
		{"refs/heads/trunk", "branch", "trunk", true},
		{"refs/heads/release/v1", "branch", "release/v1", true},
		{"refs/tags/v1.0.0", "tag", "v1.0.0", true},
		{"refs/notes/commits", "", "", false},
	}
	for _, c := range cases {
		target, name, ok := targetAndNameForRef(c.ref)
		if target != c.wantTarget || name != c.wantName || ok != c.wantOK {
			t.Fatalf("%q => (%q, %q, %v), want (%q, %q, %v)",
				c.ref, target, name, ok, c.wantTarget, c.wantName, c.wantOK)
		}
	}
}

func TestIsAllZeros(t *testing.T) {
	t.Parallel()
	if !isAllZeros("0000000000000000000000000000000000000000") {
		t.Errorf("40 zeros → true")
	}
	if isAllZeros("0000000000000000000000000000000000000001") {
		t.Errorf("39 zeros + 1 → false")
	}
	if isAllZeros("00000") {
		t.Errorf("short string → false")
	}
}
