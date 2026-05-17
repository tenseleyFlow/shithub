// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"testing"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

func TestSummarizeCodeCommitChecks(t *testing.T) {
	tests := []struct {
		name       string
		runs       []checksdb.CheckRun
		show       bool
		stateClass string
		label      string
	}{
		{
			name: "empty",
			show: false,
		},
		{
			name: "success",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionSuccess),
			},
			show:       true,
			stateClass: "success",
			label:      "1 check successful",
		},
		{
			name: "pending wins over success",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionSuccess),
				{Status: checksdb.CheckStatusInProgress},
			},
			show:       true,
			stateClass: "pending",
			label:      "1 of 2 checks pending",
		},
		{
			name: "failure wins over pending",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionFailure),
				{Status: checksdb.CheckStatusQueued},
			},
			show:       true,
			stateClass: "failure",
			label:      "1 of 2 checks failed",
		},
		{
			name: "cancelled renders distinct",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionCancelled),
			},
			show:       true,
			stateClass: "cancelled",
			label:      "1 check cancelled",
		},
		{
			name: "skipped renders distinct",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionSkipped),
			},
			show:       true,
			stateClass: "skipped",
			label:      "1 check skipped",
		},
		{
			name: "neutral renders distinct",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionNeutral),
			},
			show:       true,
			stateClass: "neutral",
			label:      "1 check neutral",
		},
		{
			name: "success wins over skipped and neutral",
			runs: []checksdb.CheckRun{
				completedCheck(checksdb.CheckConclusionSkipped),
				completedCheck(checksdb.CheckConclusionNeutral),
				completedCheck(checksdb.CheckConclusionSuccess),
			},
			show:       true,
			stateClass: "success",
			label:      "1 of 3 checks successful",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeCodeCommitChecks(tt.runs)
			if got.Show != tt.show || got.StateClass != tt.stateClass || got.Label != tt.label {
				t.Fatalf("summary: got show=%t state=%q label=%q", got.Show, got.StateClass, got.Label)
			}
		})
	}
}

func TestCodeCheckSummaryHrefUsesOnlySafeLocalDetailsURL(t *testing.T) {
	runs := []checksdb.CheckRun{
		{DetailsUrl: "https://evil.example/actions/runs/1"},
		{DetailsUrl: "//evil.example/actions/runs/2"},
		{DetailsUrl: "/mallory/other-repo/actions/runs/4"},
		{DetailsUrl: "/alice/public-repo/actions/runs/3"},
	}
	got := codeCheckSummaryHref("alice", "public-repo", runs)
	if got != "/alice/public-repo/actions/runs/3" {
		t.Fatalf("href = %q, want safe local details URL", got)
	}
}

func TestLocalActionsRunRerunHrefRequiresSameRepoActionRun(t *testing.T) {
	tests := []struct {
		name string
		href string
		want string
		ok   bool
	}{
		{name: "run", href: "/alice/public-repo/actions/runs/3", want: "/alice/public-repo/actions/runs/3/rerun", ok: true},
		{name: "wrong repo", href: "/alice/other/actions/runs/3"},
		{name: "wrong path", href: "/alice/public-repo/actions"},
		{name: "external", href: "https://evil.example/alice/public-repo/actions/runs/3"},
		{name: "non numeric run", href: "/alice/public-repo/actions/runs/latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := localActionsRunRerunHref("alice", "public-repo", tt.href)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("localActionsRunRerunHref = %q, %t; want %q, %t", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestCodeCheckSummaryHrefFallsBackToActionsList(t *testing.T) {
	runs := []checksdb.CheckRun{{DetailsUrl: ""}}
	got := codeCheckSummaryHref("alice", "public-repo", runs)
	if got != "/alice/public-repo/actions" {
		t.Fatalf("href = %q, want actions fallback", got)
	}
}

func TestUniqueHeadSHAsTrimsAndDedupes(t *testing.T) {
	got := uniqueHeadSHAs([]string{" abc ", "", "def", "abc", "def"})
	if len(got) != 2 || got[0] != "abc" || got[1] != "def" {
		t.Fatalf("uniqueHeadSHAs = %#v, want [abc def]", got)
	}
}

func completedCheck(conclusion checksdb.CheckConclusion) checksdb.CheckRun {
	return checksdb.CheckRun{
		Status: checksdb.CheckStatusCompleted,
		Conclusion: checksdb.NullCheckConclusion{
			CheckConclusion: conclusion,
			Valid:           true,
		},
	}
}
