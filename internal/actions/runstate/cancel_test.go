// SPDX-License-Identifier: AGPL-3.0-or-later

package runstate

import (
	"testing"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

func TestDeriveWorkflowRunConclusionWaitsForAllJobs(t *testing.T) {
	jobs := []actionsdb.ListJobsForRunRow{
		jobRow(actionsdb.WorkflowJobStatusCompleted, actionsdb.CheckConclusionFailure),
		{Status: actionsdb.WorkflowJobStatusQueued},
	}

	conclusion, complete := DeriveWorkflowRunConclusion(jobs)
	if complete || conclusion != "" {
		t.Fatalf("DeriveWorkflowRunConclusion() = %q, %t; want incomplete", conclusion, complete)
	}
}

func TestDeriveWorkflowRunConclusionKeepsFirstFailureAfterAllTerminal(t *testing.T) {
	jobs := []actionsdb.ListJobsForRunRow{
		jobRow(actionsdb.WorkflowJobStatusCompleted, actionsdb.CheckConclusionFailure),
		jobRow(actionsdb.WorkflowJobStatusCompleted, actionsdb.CheckConclusionSuccess),
	}

	conclusion, complete := DeriveWorkflowRunConclusion(jobs)
	if !complete || conclusion != actionsdb.CheckConclusionFailure {
		t.Fatalf("DeriveWorkflowRunConclusion() = %q, %t; want failure complete", conclusion, complete)
	}
}

func jobRow(status actionsdb.WorkflowJobStatus, conclusion actionsdb.CheckConclusion) actionsdb.ListJobsForRunRow {
	return actionsdb.ListJobsForRunRow{
		Status: status,
		Conclusion: actionsdb.NullCheckConclusion{
			CheckConclusion: conclusion,
			Valid:           true,
		},
	}
}
