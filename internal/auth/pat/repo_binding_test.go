// SPDX-License-Identifier: AGPL-3.0-or-later

package pat_test

import (
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

func TestRepoBindingAllows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		binding   int64
		requested int64
		want      bool
	}{
		{"no binding allows anything", 0, 42, true},
		{"no binding allows zero requested", 0, 0, true},
		{"binding matches", 42, 42, true},
		{"binding mismatches", 42, 43, false},
		{"bound token vs no-repo route (requested=0)", 42, 0, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := pat.RepoBindingAllows(tc.binding, tc.requested); got != tc.want {
				t.Errorf("RepoBindingAllows(%d, %d) = %v, want %v",
					tc.binding, tc.requested, got, tc.want)
			}
		})
	}
}
