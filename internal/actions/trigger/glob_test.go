// SPDX-License-Identifier: AGPL-3.0-or-later

package trigger

import "testing"

// Tests live in the same package so we can exercise unexported
// matchAny + globMatch directly. The trigger package's public API
// (Match, Discover, Enqueue) gets _test.go files in package
// trigger_test where the surface should be opaque.

func TestGlobMatch_LiteralAndStar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"main", "main", true},
		{"main", "feature/foo", false},
		{"main", "main/sub", false},
		{"feature/*", "feature/foo", true},
		{"feature/*", "feature/foo/bar", false}, // * doesn't cross /
		{"feature/*", "feature/", true},         // trailing-empty acceptable
		{"*", "anything", true},
		{"*", "with/slash", false},
		{"*.tar.gz", "foo.tar.gz", true},
		{"*.tar.gz", "foo/bar.tar.gz", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.s, func(t *testing.T) {
			t.Parallel()
			got := globMatch(tc.pattern, tc.s)
			if got != tc.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
			}
		})
	}
}

func TestGlobMatch_DoubleStar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"feature/**", "feature", true}, // zero trailing segments
		{"feature/**", "feature/foo", true},
		{"feature/**", "feature/foo/bar", true},
		{"feature/**", "main", false},
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/sub/x.go", true},
		{"**/*.go", "pkg/sub/x.txt", false},
		{"docs/**/*.md", "docs/internal/x.md", true},
		{"docs/**/*.md", "docs/x.md", true}, // ** matches zero segments
		{"docs/**/*.md", "src/x.md", false},
		{"**", "literally/any/path", true},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"_"+tc.s, func(t *testing.T) {
			t.Parallel()
			got := globMatch(tc.pattern, tc.s)
			if got != tc.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
			}
		})
	}
}

// TestMatchAny_MixedIncludeExclude pins the GHA-style include + `!exclude`
// semantics: last match wins in declaration order, but a list of *only*
// exclusions implicitly includes everything not excluded.
func TestMatchAny_MixedIncludeExclude(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		patterns []string
		s        string
		want     bool
	}{
		{"empty list matches all", nil, "anything", true},
		{"single include matches", []string{"main"}, "main", true},
		{"single include miss", []string{"main"}, "feature/foo", false},
		{"include + exclude — included wins", []string{"feature/**", "!feature/skip"}, "feature/foo", true},
		{"include + exclude — excluded loses", []string{"feature/**", "!feature/skip"}, "feature/skip", false},
		{"only-exclusions implicit-include", []string{"!main"}, "feature/foo", true},
		{"only-exclusions hit", []string{"!main"}, "main", false},
		{"order matters — last-include re-includes", []string{"feature/**", "!feature/skip", "feature/skip"}, "feature/skip", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := matchAny(tc.patterns, tc.s)
			if got != tc.want {
				t.Errorf("matchAny(%v, %q) = %v, want %v", tc.patterns, tc.s, got, tc.want)
			}
		})
	}
}
