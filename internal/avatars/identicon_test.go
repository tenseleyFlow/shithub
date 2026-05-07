// SPDX-License-Identifier: AGPL-3.0-or-later

package avatars

import (
	"strings"
	"testing"
)

func TestIdenticon_Deterministic(t *testing.T) {
	t.Parallel()
	a := Identicon("alice", 80)
	b := Identicon("alice", 80)
	if a != b {
		t.Fatal("Identicon must be deterministic for the same username")
	}
}

func TestIdenticon_DifferentUsers(t *testing.T) {
	t.Parallel()
	a := Identicon("alice", 80)
	b := Identicon("bob", 80)
	if a == b {
		t.Fatal("different usernames should produce different SVGs")
	}
}

func TestIdenticon_CaseInsensitive(t *testing.T) {
	t.Parallel()
	if Identicon("Alice", 80) != Identicon("alice", 80) {
		t.Fatal("identicon should not change with username case")
	}
}

func TestIdenticon_ContainsSVGSkeleton(t *testing.T) {
	t.Parallel()
	out := Identicon("alice", 64)
	for _, want := range []string{
		`<svg `, `</svg>`, `viewBox="0 0 5 5"`, `<rect`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in svg", want)
		}
	}
}

func TestIdenticon_Symmetry(t *testing.T) {
	t.Parallel()
	out := Identicon("symmetry", 80)
	// For each <rect x="0" ..> we expect a corresponding <rect x="4" ..>.
	leftCount := strings.Count(out, `x="0"`)
	rightCount := strings.Count(out, `x="4"`)
	if leftCount != rightCount {
		t.Fatalf("asymmetric: left col rects=%d, right col rects=%d", leftCount, rightCount)
	}
}
