// SPDX-License-Identifier: AGPL-3.0-or-later

// Package statuspage builds the data Pro users see at /<username>/status
// and the SVG badge at /<username>.status.svg (PRO-EXT01-14).
//
// Aggregation shape: for each pinned repo, summarise the most-recent
// completed workflow run on the default branch + a 30-day success
// rate. Overall state is one of ok / degraded / down / unknown based
// on the per-repo conclusions:
//
//   - ok: every pinned repo's latest run on the default branch
//     succeeded.
//   - degraded: at least one pinned repo had a recent success and at
//     least one had a recent non-success.
//   - down: every pinned repo's latest run failed.
//   - unknown: no pinned repos OR no completed runs in the lookback
//     window.
//
// The aggregator queries existing tables read-only — no new schema.
// LookbackWindow caps the 30-day success-rate calculation.
package statuspage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// State enumerates the four account-level outcomes.
type State string

const (
	StateOK       State = "ok"
	StateDegraded State = "degraded"
	StateDown     State = "down"
	StateUnknown  State = "unknown"
)

// LookbackWindow bounds the per-repo success-rate computation. 30
// days matches the gh Insights default; long enough to smooth noise
// from a single flaky run, short enough to reflect recent reality.
const LookbackWindow = 30 * 24 * time.Hour

// PerRepoRunCap caps how many runs we examine per repo. Set so the
// aggregator's cost is bounded for users with very chatty CI.
const PerRepoRunCap = 100

// Summary is the aggregated status — a single OverallState + one
// RepoStatus per pinned repo (in pin position order).
type Summary struct {
	OverallState State
	Repos        []RepoStatus
	GeneratedAt  time.Time
}

// RepoStatus is the per-repo entry. SuccessRate is 0.0–1.0; -1 when
// no completed runs exist in the lookback window.
type RepoStatus struct {
	RepoID        int64
	Owner         string
	Name          string
	DefaultBranch string
	LatestRun     LatestRun
	SuccessRate   float64 // [0, 1], or -1 for "no data"
	TotalRuns     int     // runs counted in SuccessRate (≤ PerRepoRunCap)
}

// LatestRun captures the most recent COMPLETED run on the default
// branch. Conclusion is empty when no completed run exists.
type LatestRun struct {
	Conclusion  string
	CompletedAt time.Time
	RunIndex    int64
}

// Aggregate computes the Summary for `userID`. Returns a Summary with
// OverallState=unknown + empty Repos if the user has no pinned repos
// (a Pro user with an empty pin set still gets a renderable page).
func Aggregate(ctx context.Context, pool *pgxpool.Pool, userID int64, ownerUsername string) (Summary, error) {
	if pool == nil {
		return Summary{}, errors.New("statuspage: nil pool")
	}
	repoIDs, err := pinnedRepoIDs(ctx, pool, userID)
	if err != nil {
		return Summary{}, err
	}
	out := Summary{
		OverallState: StateUnknown,
		Repos:        make([]RepoStatus, 0, len(repoIDs)),
		GeneratedAt:  time.Now().UTC(),
	}
	if len(repoIDs) == 0 {
		return out, nil
	}

	q := reposdb.New()
	aq := actionsdb.New()
	cutoff := time.Now().Add(-LookbackWindow)

	for _, repoID := range repoIDs {
		repo, err := q.GetRepoByID(ctx, pool, repoID)
		if err != nil {
			// Skip a row that disappeared between pin-list and
			// repo-fetch — the user's status page shouldn't 500
			// over a phantom pin.
			continue
		}
		rs := RepoStatus{
			RepoID:        repo.ID,
			Owner:         ownerUsername,
			Name:          repo.Name,
			DefaultBranch: repo.DefaultBranch,
			SuccessRate:   -1,
		}
		runs, err := aq.ListWorkflowRunsForRepo(ctx, pool, actionsdb.ListWorkflowRunsForRepoParams{
			RepoID:    repoID,
			HeadRef:   pgtype.Text{String: "refs/heads/" + repo.DefaultBranch, Valid: true},
			PageLimit: PerRepoRunCap,
		})
		if err != nil {
			out.Repos = append(out.Repos, rs)
			continue
		}
		populateRepoStatus(&rs, runs, cutoff)
		out.Repos = append(out.Repos, rs)
	}

	out.OverallState = computeOverall(out.Repos)
	return out, nil
}

// pinnedRepoIDs reads the user's pin set then the pins; returns IDs
// in pin position order. Two-query shape matches the existing pin
// handler (set lookup then list). Empty result when no pin set or
// no pins.
func pinnedRepoIDs(ctx context.Context, pool *pgxpool.Pool, userID int64) ([]int64, error) {
	q := reposdb.New()
	setID, err := q.GetProfilePinSetForUser(ctx, pool, pgtype.Int8{Int64: userID, Valid: true})
	if err != nil {
		// pgx's no-rows isn't an error we propagate — a user without
		// a pin set just has no pins.
		return nil, nil
	}
	rows, err := q.ListProfilePinsForSet(ctx, pool, setID)
	if err != nil {
		return nil, fmt.Errorf("statuspage: list pins: %w", err)
	}
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.RepoID
	}
	return out, nil
}

// populateRepoStatus fills LatestRun + SuccessRate + TotalRuns from
// the run list (which is newest-first).
func populateRepoStatus(rs *RepoStatus, runs []actionsdb.ListWorkflowRunsForRepoRow, cutoff time.Time) {
	var (
		successes int
		totals    int
		latestSet bool
	)
	for _, run := range runs {
		// Only completed runs count toward the success rate.
		if run.Status != actionsdb.WorkflowRunStatusCompleted {
			continue
		}
		if run.CompletedAt.Valid && run.CompletedAt.Time.Before(cutoff) {
			// Past the lookback window — newer runs come first, so
			// further runs are also out of window; break.
			break
		}
		if !latestSet {
			rs.LatestRun = LatestRun{
				Conclusion:  conclusionString(run.Conclusion),
				CompletedAt: run.CompletedAt.Time,
				RunIndex:    run.RunIndex,
			}
			latestSet = true
		}
		totals++
		if run.Conclusion.Valid &&
			run.Conclusion.CheckConclusion == actionsdb.CheckConclusionSuccess {
			successes++
		}
	}
	if totals > 0 {
		rs.SuccessRate = float64(successes) / float64(totals)
		rs.TotalRuns = totals
	}
}

// computeOverall maps per-repo latest-run conclusions to a single
// account-level state. The rule is "majority wins, with degraded as
// the mixed-signal fallback" — a single failure across many repos
// shouldn't tank the whole page from ok → down.
func computeOverall(repos []RepoStatus) State {
	if len(repos) == 0 {
		return StateUnknown
	}
	var (
		anyData    bool
		anyFailure bool
		anyOK      bool
	)
	for _, r := range repos {
		if r.LatestRun.Conclusion == "" {
			continue
		}
		anyData = true
		switch r.LatestRun.Conclusion {
		case string(actionsdb.CheckConclusionSuccess):
			anyOK = true
		case string(actionsdb.CheckConclusionFailure),
			string(actionsdb.CheckConclusionTimedOut):
			anyFailure = true
		}
	}
	switch {
	case !anyData:
		return StateUnknown
	case anyOK && !anyFailure:
		return StateOK
	case !anyOK && anyFailure:
		return StateDown
	default:
		return StateDegraded
	}
}

func conclusionString(c actionsdb.NullCheckConclusion) string {
	if !c.Valid {
		return ""
	}
	return string(c.CheckConclusion)
}
