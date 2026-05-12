// SPDX-License-Identifier: AGPL-3.0-or-later

// Package runstate owns shared workflow-run status derivation helpers.
package runstate

import (
	"context"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

// RollupAfterCancel updates the parent workflow_run after one or more jobs in
// the run received a cancellation request. If all jobs are terminal, the run is
// completed with the derived conclusion; otherwise it is marked running.
func RollupAfterCancel(
	ctx context.Context,
	q *actionsdb.Queries,
	db actionsdb.DBTX,
	runID int64,
) (bool, actionsdb.CheckConclusion, error) {
	jobs, err := q.ListJobsForRun(ctx, db, runID)
	if err != nil {
		return false, "", err
	}
	runConclusion, complete := DeriveWorkflowRunConclusion(jobs)
	if complete {
		if _, err := q.CompleteWorkflowRun(ctx, db, actionsdb.CompleteWorkflowRunParams{
			ID:         runID,
			Conclusion: runConclusion,
		}); err != nil {
			return false, "", err
		}
		return true, runConclusion, nil
	}
	if err := q.MarkWorkflowRunRunning(ctx, db, runID); err != nil {
		return false, "", err
	}
	return false, "", nil
}

// DeriveWorkflowRunConclusion mirrors GitHub's "worst job wins" rollup for
// the conclusion set shithub currently supports.
func DeriveWorkflowRunConclusion(jobs []actionsdb.ListJobsForRunRow) (actionsdb.CheckConclusion, bool) {
	if len(jobs) == 0 {
		return actionsdb.CheckConclusionFailure, true
	}
	worst := actionsdb.CheckConclusionSuccess
	for _, job := range jobs {
		switch job.Status {
		case actionsdb.WorkflowJobStatusCompleted, actionsdb.WorkflowJobStatusCancelled, actionsdb.WorkflowJobStatusSkipped:
		default:
			return "", false
		}
		if job.Status == actionsdb.WorkflowJobStatusCancelled {
			worst = actionsdb.CheckConclusionCancelled
			continue
		}
		if !job.Conclusion.Valid {
			return actionsdb.CheckConclusionFailure, true
		}
		c := job.Conclusion.CheckConclusion
		if c == actionsdb.CheckConclusionFailure ||
			c == actionsdb.CheckConclusionTimedOut ||
			c == actionsdb.CheckConclusionActionRequired {
			return c, true
		}
		if c == actionsdb.CheckConclusionCancelled {
			worst = actionsdb.CheckConclusionCancelled
		}
	}
	return worst, true
}
