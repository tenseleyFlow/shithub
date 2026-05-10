// SPDX-License-Identifier: AGPL-3.0-or-later

package search

import (
	"net/http/httptest"
	"testing"
)

func TestPageFromRequestUsesGitHubStylePParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/search?q=repo&p=3", nil)
	if got := pageFromRequest(req); got != 3 {
		t.Fatalf("pageFromRequest = %d, want 3", got)
	}
}

func TestPageFromRequestAcceptsLegacyPageParam(t *testing.T) {
	req := httptest.NewRequest("GET", "/search?q=repo&page=2", nil)
	if got := pageFromRequest(req); got != 2 {
		t.Fatalf("pageFromRequest = %d, want 2", got)
	}
}

func TestSearchHrefEscapesQuery(t *testing.T) {
	got := searchHref("repo:alice/demo pub", "issues", 4)
	want := "/search?p=4&q=repo%3Aalice%2Fdemo+pub&type=issues"
	if got != want {
		t.Fatalf("searchHref = %q, want %q", got, want)
	}
}

func TestNormalizeSearchTabAcceptsGitHubTypesAndLegacyAliases(t *testing.T) {
	cases := map[string]string{
		"":              "repositories",
		"repos":         "repositories",
		"repositories":  "repositories",
		"pulls":         "pullrequests",
		"pullrequests":  "pullrequests",
		"code":          "code",
		"issues":        "issues",
		"users":         "users",
		"unknown-value": "repositories",
	}
	for input, want := range cases {
		if got := normalizeSearchTab(input); got != want {
			t.Fatalf("normalizeSearchTab(%q) = %q, want %q", input, got, want)
		}
	}
}
