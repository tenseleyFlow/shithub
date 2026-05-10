// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/repos/git"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func TestRepoAboutResources_GitHubResourceOrder(t *testing.T) {
	t.Parallel()
	row := reposdb.Repo{LicenseKey: pgtype.Text{String: "AGPL-3.0", Valid: true}}
	entries := []git.TreeEntry{
		{Kind: git.EntryBlob, Name: "README.md"},
		{Kind: git.EntryBlob, Name: "LICENSE"},
		{Kind: git.EntryBlob, Name: "CODE_OF_CONDUCT.md"},
		{Kind: git.EntryBlob, Name: "CONTRIBUTING.md"},
		{Kind: git.EntryBlob, Name: "SECURITY.md"},
	}

	got := repoAboutResources("tenseleyFlow", "shithub", "trunk", row, entries)
	labels := make([]string, 0, len(got))
	for _, r := range got {
		labels = append(labels, r.Label)
	}
	want := []string{
		"Readme",
		"AGPL-3.0 license",
		"Code of conduct",
		"Contributing",
		"Security policy",
		"Activity",
		"Custom properties",
	}
	if len(labels) != len(want) {
		t.Fatalf("labels = %#v, want %#v", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Fatalf("labels = %#v, want %#v", labels, want)
		}
	}
	if got[1].Href != "/tenseleyFlow/shithub/blob/trunk/LICENSE" {
		t.Fatalf("license href = %q", got[1].Href)
	}
}

func TestRepoLanguageForPath_ApproximatesGitHubLinguist(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"internal/web/server.go", "Go", true},
		{"templates/repo/tree.html", "HTML", true},
		{"static/css/shithub.css", "CSS", true},
		{"scripts/dev.sh", "Shell", true},
		{"deploy/schema.sql", "PLpgSQL", true},
		{"README.md", "", false},
		{"LICENSE", "", false},
	}
	for _, tc := range cases {
		got, _, ok := repoLanguageForPath(tc.path)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%s: got (%q, %v), want (%q, %v)", tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestMergeSmallLanguages_CapsSidebarList(t *testing.T) {
	t.Parallel()
	ordered := []repoLanguageAggregate{
		{name: "Go", size: 100},
		{name: "HTML", size: 90},
		{name: "CSS", size: 80},
		{name: "Shell", size: 70},
		{name: "PLpgSQL", size: 60},
		{name: "Jinja", size: 50},
		{name: "JavaScript", size: 40},
		{name: "Python", size: 30},
	}
	got := mergeSmallLanguages(ordered)
	if len(got) != 7 {
		t.Fatalf("len = %d, want 7: %#v", len(got), got)
	}
	if got[6].name != "Other" || got[6].size != 70 {
		t.Fatalf("merged tail = %#v, want Other size 70", got[6])
	}
}
