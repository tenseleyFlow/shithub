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
	if got[0].Href != "/tenseleyFlow/shithub?tab=readme-ov-file#readme" || got[0].Path != "README.md" || got[0].OverviewTab != repoOverviewReadmeTab {
		t.Fatalf("readme resource = %#v", got[0])
	}
	if got[1].Href != "/tenseleyFlow/shithub?tab=license-ov-file#readme" || got[1].Path != "LICENSE" || got[1].OverviewTab != repoOverviewLicenseTab {
		t.Fatalf("license href = %q", got[1].Href)
	}
}

func TestRepoReadmeTabs_FiltersDocumentTabs(t *testing.T) {
	t.Parallel()
	resources := []repoAboutResource{
		{Icon: "book", Label: "Readme", Href: "/demo?tab=readme-ov-file", Path: "README.md", OverviewTab: repoOverviewReadmeTab},
		{Icon: "law", Label: "AGPL-3.0 license", Href: "/demo?tab=license-ov-file", Path: "LICENSE", OverviewTab: repoOverviewLicenseTab},
		{Icon: "heart", Label: "Code of conduct", Href: "/demo?tab=coc-ov-file", Path: "CODE_OF_CONDUCT.md", OverviewTab: repoOverviewCodeOfConductTab},
		{Icon: "people", Label: "Contributing", Href: "/demo?tab=contributing-ov-file", Path: "CONTRIBUTING.md", OverviewTab: repoOverviewContributingTab},
		{Icon: "law", Label: "Security policy", Href: "/demo?tab=security-ov-file", Path: "SECURITY.md", OverviewTab: repoOverviewSecurityTab},
		{Icon: "pulse", Label: "Activity", Href: "/activity"},
		{Icon: "note", Label: "Custom properties", Href: "/settings/custom-properties"},
	}
	got := repoReadmeTabs(resources, repoOverviewContributingTab)
	want := []string{"README", "AGPL-3.0 license", "Code of conduct", "Contributing", "Security"}
	if len(got) != len(want) {
		t.Fatalf("tabs = %#v, want labels %#v", got, want)
	}
	for i := range want {
		if got[i].Label != want[i] {
			t.Fatalf("tabs = %#v, want labels %#v", got, want)
		}
	}
	if got[3].Href != "/demo?tab=contributing-ov-file" || !got[3].Active {
		t.Fatalf("Contributing tab should be active and link to overview query: %#v", got[3])
	}
	if got[0].Active {
		t.Fatalf("README tab should not be active: %#v", got[0])
	}
}

func TestActiveRepoOverviewResource_DefaultsToReadme(t *testing.T) {
	t.Parallel()
	resources := []repoAboutResource{
		{Icon: "people", Label: "Contributing", Href: "/demo?tab=contributing-ov-file", Path: "CONTRIBUTING.md", OverviewTab: repoOverviewContributingTab},
		{Icon: "book", Label: "Readme", Href: "/demo?tab=readme-ov-file", Path: "README.md", OverviewTab: repoOverviewReadmeTab},
	}
	got, ok := activeRepoOverviewResource(resources, "")
	if !ok || got.Path != "README.md" {
		t.Fatalf("default active resource = %#v, %v", got, ok)
	}
	got, ok = activeRepoOverviewResource(resources, repoOverviewContributingTab)
	if !ok || got.Path != "CONTRIBUTING.md" {
		t.Fatalf("requested active resource = %#v, %v", got, ok)
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
