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

func completedCheck(conclusion checksdb.CheckConclusion) checksdb.CheckRun {
	return checksdb.CheckRun{
		Status: checksdb.CheckStatusCompleted,
		Conclusion: checksdb.NullCheckConclusion{
			CheckConclusion: conclusion,
			Valid:           true,
		},
	}
}
