// SPDX-License-Identifier: AGPL-3.0-or-later

package statuspage

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
)

// TestComputeOverall exercises the per-repo → account-level rollup
// rules independently of any DB. The truth table is the load-bearing
// part of the aggregator — get it wrong and every Pro user's status
// badge lies.
func TestComputeOverall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		repos []RepoStatus
		want  State
	}{
		{
			name:  "no repos at all",
			repos: nil,
			want:  StateUnknown,
		},
		{
			name: "all empty conclusions = unknown",
			repos: []RepoStatus{
				{LatestRun: LatestRun{Conclusion: ""}},
				{LatestRun: LatestRun{Conclusion: ""}},
			},
			want: StateUnknown,
		},
		{
			name: "all success = ok",
			repos: []RepoStatus{
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionSuccess)}},
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionSuccess)}},
			},
			want: StateOK,
		},
		{
			name: "all failure = down",
			repos: []RepoStatus{
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionFailure)}},
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionTimedOut)}},
			},
			want: StateDown,
		},
		{
			name: "mixed = degraded",
			repos: []RepoStatus{
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionSuccess)}},
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionFailure)}},
			},
			want: StateDegraded,
		},
		{
			name: "success + neutral (ignored) = ok",
			repos: []RepoStatus{
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionSuccess)}},
				{LatestRun: LatestRun{Conclusion: string(actionsdb.CheckConclusionNeutral)}},
			},
			want: StateOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := computeOverall(tc.repos); got != tc.want {
				t.Errorf("computeOverall = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestPopulateRepoStatus_HappyPath asserts that newest-first runs are
// counted toward the success rate and the latest completed run is
// surfaced.
func TestPopulateRepoStatus_HappyPath(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	runs := []actionsdb.ListWorkflowRunsForRepoRow{
		mkRun(now.Add(-1*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionSuccess, 100),
		mkRun(now.Add(-2*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionFailure, 99),
		mkRun(now.Add(-3*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionSuccess, 98),
	}
	var rs RepoStatus
	rs.SuccessRate = -1
	populateRepoStatus(&rs, runs, cutoff)

	if rs.LatestRun.RunIndex != 100 {
		t.Errorf("LatestRun.RunIndex = %d, want 100", rs.LatestRun.RunIndex)
	}
	if rs.LatestRun.Conclusion != "success" {
		t.Errorf("LatestRun.Conclusion = %q, want success", rs.LatestRun.Conclusion)
	}
	if rs.TotalRuns != 3 {
		t.Errorf("TotalRuns = %d, want 3", rs.TotalRuns)
	}
	if rs.SuccessRate < 0.66 || rs.SuccessRate > 0.67 {
		t.Errorf("SuccessRate = %f, want ~0.667", rs.SuccessRate)
	}
}

// TestPopulateRepoStatus_BreaksAtCutoff is the bound on the
// aggregator's cost: runs older than the cutoff stop iteration
// because the list is newest-first. If the break is removed by a
// refactor this test fails.
func TestPopulateRepoStatus_BreaksAtCutoff(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	runs := []actionsdb.ListWorkflowRunsForRepoRow{
		mkRun(now.Add(-1*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionSuccess, 50),
		// Past the window — should stop counting here.
		mkRun(now.Add(-40*24*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionFailure, 49),
		mkRun(now.Add(-41*24*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionFailure, 48),
	}
	var rs RepoStatus
	rs.SuccessRate = -1
	populateRepoStatus(&rs, runs, cutoff)
	if rs.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1 (cutoff break)", rs.TotalRuns)
	}
	if rs.SuccessRate != 1.0 {
		t.Errorf("SuccessRate = %f, want 1.0", rs.SuccessRate)
	}
}

// TestPopulateRepoStatus_SkipsIncomplete asserts queued/running runs
// don't count — only completed runs do. A regression that includes
// in-flight runs would noise the 30-day rate.
func TestPopulateRepoStatus_SkipsIncomplete(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	runs := []actionsdb.ListWorkflowRunsForRepoRow{
		mkRun(now.Add(-30*time.Minute), actionsdb.WorkflowRunStatusQueued, actionsdb.CheckConclusionSuccess, 10),
		mkRun(now.Add(-1*time.Hour), actionsdb.WorkflowRunStatusCompleted, actionsdb.CheckConclusionSuccess, 9),
	}
	var rs RepoStatus
	rs.SuccessRate = -1
	populateRepoStatus(&rs, runs, cutoff)
	if rs.TotalRuns != 1 {
		t.Errorf("TotalRuns = %d, want 1 (queued skipped)", rs.TotalRuns)
	}
	if rs.LatestRun.RunIndex != 9 {
		t.Errorf("LatestRun.RunIndex = %d, want 9 (queued must not be latest)", rs.LatestRun.RunIndex)
	}
}

// TestPopulateRepoStatus_NoData leaves SuccessRate at -1 sentinel so
// the template renders "—" instead of "0%".
func TestPopulateRepoStatus_NoData(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	var rs RepoStatus
	rs.SuccessRate = -1
	populateRepoStatus(&rs, nil, cutoff)
	if rs.SuccessRate != -1 {
		t.Errorf("SuccessRate = %f, want -1 sentinel", rs.SuccessRate)
	}
	if rs.TotalRuns != 0 {
		t.Errorf("TotalRuns = %d, want 0", rs.TotalRuns)
	}
}

// mkRun builds a synthetic ListWorkflowRunsForRepoRow so the
// populateRepoStatus tests don't have to set up the DB.
func mkRun(completed time.Time, status actionsdb.WorkflowRunStatus, conclusion actionsdb.CheckConclusion, idx int64) actionsdb.ListWorkflowRunsForRepoRow {
	return actionsdb.ListWorkflowRunsForRepoRow{
		RunIndex:    idx,
		Status:      status,
		Conclusion:  actionsdb.NullCheckConclusion{CheckConclusion: conclusion, Valid: true},
		CompletedAt: pgtype.Timestamptz{Time: completed, Valid: true},
	}
}
