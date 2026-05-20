// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import "testing"

func TestRepoScopedIssueQueryDefaultsAndScopes(t *testing.T) {
	parsed, state := repoScopedIssueQuery("author:esp label:bug", "", "tenseleyFlow", "shithub")
	if state != "open" {
		t.Fatalf("state = %q, want open", state)
	}
	if parsed.StateFilter != "open" {
		t.Fatalf("parsed state = %q, want open", parsed.StateFilter)
	}
	if parsed.RepoFilter == nil || parsed.RepoFilter.Owner != "tenseleyFlow" || parsed.RepoFilter.Name != "shithub" {
		t.Fatalf("repo filter = %#v, want tenseleyFlow/shithub", parsed.RepoFilter)
	}
	if parsed.AuthorFilter != "esp" {
		t.Fatalf("author = %q, want esp", parsed.AuthorFilter)
	}
	if len(parsed.LabelFilters) != 1 || parsed.LabelFilters[0] != "bug" {
		t.Fatalf("labels = %#v, want bug", parsed.LabelFilters)
	}
}

func TestRepoScopedIssueQueryPreservesQueryStateAndOverridesRepo(t *testing.T) {
	parsed, state := repoScopedIssueQuery("repo:elsewhere/other is:closed is:pr", "", "tenseleyFlow", "shithub")
	if state != "closed" {
		t.Fatalf("state = %q, want closed", state)
	}
	if parsed.StateFilter != "closed" {
		t.Fatalf("parsed state = %q, want closed", parsed.StateFilter)
	}
	if parsed.KindFilter != "pr" {
		t.Fatalf("kind = %q, want pr", parsed.KindFilter)
	}
	if parsed.RepoFilter == nil || parsed.RepoFilter.Owner != "tenseleyFlow" || parsed.RepoFilter.Name != "shithub" {
		t.Fatalf("repo filter = %#v, want tenseleyFlow/shithub", parsed.RepoFilter)
	}
}
