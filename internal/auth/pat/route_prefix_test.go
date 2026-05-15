// SPDX-License-Identifier: AGPL-3.0-or-later

package pat_test

import (
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

func TestRoutePrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"", "/"},
		{"/", "/"},
		{"/api/v1/user", "/api/v1/user"},
		{"/api/v1/repos/alice/demo", "/api/v1/repos"},
		{"/api/v1/repos/alice/demo/issues/42/comments", "/api/v1/repos"},
		// 4-segment paths truncate to the 3-segment cap regardless of
		// segment shape — shithub's API surface puts identifiers at
		// segment 4+, so the cap is sufficient for analytics buckets.
		{"/api/v1/repos/alice", "/api/v1/repos"},
		{"/api/v1/issues/42", "/api/v1/issues"},
		{"/api/v1/commits/" + strings.Repeat("a", 40), "/api/v1/commits"},
		{"/api/v1/users/cafe", "/api/v1/users"},
		// Numeric 3rd segment within the cap still gets canonicalized.
		{"/orgs/42", "/orgs/:id"},
		// Uppercase normalizes.
		{"/API/V1/USER", "/api/v1/user"},
		// Trailing slash.
		{"/api/v1/user/", "/api/v1/user"},
	}
	for _, tc := range cases {
		if got := pat.RoutePrefix(tc.in); got != tc.want {
			t.Errorf("RoutePrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRoutePrefix_RespectsMaxBytes(t *testing.T) {
	t.Parallel()
	// Construct a path whose first three segments together exceed 64
	// bytes; canonicalization (lower-case) won't shrink them.
	// Use 'g'/'h'/'i' so isHex doesn't kick in and turn each segment into :id.
	long := "/" + strings.Repeat("g", 25) + "/" + strings.Repeat("h", 25) + "/" + strings.Repeat("i", 25)
	got := pat.RoutePrefix(long)
	if len(got) > 64 {
		t.Errorf("RoutePrefix returned %d bytes (want <=64): %q", len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("oversized result should carry truncation marker: %q", got)
	}
}
