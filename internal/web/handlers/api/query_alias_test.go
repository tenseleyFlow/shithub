// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http/httptest"
	"testing"
)

// G1: firstQueryParam returns the first non-empty trimmed alias. Pins
// the precedence rule used by issues/pulls list handlers — author wins
// over creator when both are sent so a caller migrating wire shapes
// doesn't accidentally double-up.
func TestFirstQueryParam(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		probe []string
		want  string
	}{
		{"single match", "author=alice", []string{"author", "creator"}, "alice"},
		{"alias match", "creator=bob", []string{"author", "creator"}, "bob"},
		{"first wins", "author=alice&creator=bob", []string{"author", "creator"}, "alice"},
		{"empty trimmed", "author=%20%20&creator=carol", []string{"author", "creator"}, "carol"},
		{"none present", "state=open", []string{"author", "creator"}, ""},
		{"all blank", "author=&creator=", []string{"author", "creator"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/?"+tc.query, nil)
			got := firstQueryParam(r, tc.probe...)
			if got != tc.want {
				t.Errorf("firstQueryParam(%q, %v) = %q, want %q", tc.query, tc.probe, got, tc.want)
			}
		})
	}
}
