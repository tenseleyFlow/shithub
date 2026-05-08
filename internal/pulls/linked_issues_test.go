// SPDX-License-Identifier: AGPL-3.0-or-later

package pulls

import (
	"reflect"
	"testing"
)

func TestParseLinkedIssues(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want []int64
	}{
		{"closes", "closes #1", []int64{1}},
		{"fixes_capitalized", "Fixes #42", []int64{42}},
		{"resolves_past_tense", "Resolved #7", []int64{7}},
		{"multiple", "Closes #1, fixes #2, resolves #3", []int64{1, 2, 3}},
		{"dedup", "Fixes #5\nFixes #5 again", []int64{5}},
		{"plain_hash_does_not_match", "see #99 — does not auto-close", nil},
		{"keyword_in_prose_no_hash", "this closes the door on bug fixes", nil},
		{"fixed_with_extra_whitespace", "Fixed  #11", []int64{11}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseLinkedIssues(c.body)
			if !reflect.DeepEqual(got, c.want) && !(len(got) == 0 && len(c.want) == 0) {
				t.Errorf("body %q: got %v, want %v", c.body, got, c.want)
			}
		})
	}
}
