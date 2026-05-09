// SPDX-License-Identifier: AGPL-3.0-or-later

package issues

import (
	"reflect"
	"sort"
	"testing"
)

// findSameRepoRefs runs the regex used by the same-repo branch and
// returns the parsed numbers. It mirrors the production loop modulo the
// DB lookup, so the regex behavior can be unit-tested without a DB.
func findSameRepoRefs(body string) []string {
	stripped := reCrossRepoIssueRef.ReplaceAllString(body, " ")
	out := []string{}
	for _, m := range reSameRepoIssueRef.FindAllStringSubmatch(stripped, -1) {
		out = append(out, m[1])
	}
	return out
}

func findCrossRepoRefs(body string) [][3]string {
	out := [][3]string{}
	for _, m := range reCrossRepoIssueRef.FindAllStringSubmatch(body, -1) {
		out = append(out, [3]string{m[1], m[2], m[3]})
	}
	return out
}

func TestSameRepoRefRegex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"plain", "fixes #1 and #42", []string{"1", "42"}},
		{"sentence_start", "#7 should land", []string{"7"}},
		{"no_word_prefix", "abc#1 isn't a ref", []string{}},
		{"trailing_punct", "see #3, please", []string{"3"}},
		{"line_start", "Done.\n#5 next", []string{"5"}},
		// owner/repo#N must NOT also produce a same-repo hit on the #N
		// portion; the cross-repo regex strips the whole token first.
		{"cross_repo_excluded", "alice/repo#9 only", []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := findSameRepoRefs(c.body)
			sort.Strings(got)
			sort.Strings(c.want)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("body %q: got %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestCrossRepoRefRegex(t *testing.T) {
	t.Parallel()
	got := findCrossRepoRefs("see alice/proj#3 and bob/lib#42 for context, but not just #1")
	want := [][3]string{
		{"alice", "proj", "3"},
		{"bob", "lib", "42"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
