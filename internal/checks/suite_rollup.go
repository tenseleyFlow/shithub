// SPDX-License-Identifier: AGPL-3.0-or-later

package checks

import (
	"context"

	"github.com/jackc/pgx/v5"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

// rollupSuiteInTx recomputes the suite's status + conclusion from its
// runs and persists the result. Called from Create and Update inside
// the same tx so the API response reflects the latest derived state.
//
// Per spec design table:
//
//	status = 'completed' iff every run is completed
//	conclusion priority (when status='completed'):
//	    failure         > timed_out > cancelled > action_required >
//	    success         > neutral > skipped     > stale
//
// The "first failure-class wins" ordering matches GitHub's roll-up.
// Any non-completed run forces status to 'in_progress' (or keeps the
// existing 'queued' if every run is queued — we treat queued+completed
// as 'in_progress' too, since *something* moved).
func rollupSuiteInTx(ctx context.Context, tx pgx.Tx, suiteID int64) error {
	q := checksdb.New()
	runs, err := q.ListCheckRunsBySuite(ctx, tx, suiteID)
	if err != nil {
		return err
	}
	status, conclusion := DeriveSuiteRollup(runs)
	conclusionParam := checksdb.NullCheckConclusion{}
	if conclusion != "" {
		conclusionParam = checksdb.NullCheckConclusion{
			CheckConclusion: checksdb.CheckConclusion(conclusion),
			Valid:           true,
		}
	}
	return q.UpdateCheckSuiteRollup(ctx, tx, checksdb.UpdateCheckSuiteRollupParams{
		ID:         suiteID,
		Status:     checksdb.CheckStatus(status),
		Conclusion: conclusionParam,
	})
}

// DeriveSuiteRollup is the pure-function form of the rollup so tests
// can exercise the priority order without a DB. Public so the API
// layer can preview the rollup if needed.
func DeriveSuiteRollup(runs []checksdb.CheckRun) (status, conclusion string) {
	if len(runs) == 0 {
		return "queued", ""
	}
	allCompleted := true
	anyMoved := false
	for _, r := range runs {
		if r.Status != checksdb.CheckStatusCompleted {
			allCompleted = false
		}
		if r.Status != checksdb.CheckStatusQueued {
			anyMoved = true
		}
	}
	if !allCompleted {
		if anyMoved {
			return "in_progress", ""
		}
		return "queued", ""
	}
	// Every run completed → derive aggregate conclusion.
	priority := []string{
		"failure", "timed_out", "cancelled", "action_required",
		"success", "neutral", "skipped", "stale",
	}
	rank := map[string]int{}
	for i, p := range priority {
		rank[p] = i
	}
	bestIdx := -1
	for _, r := range runs {
		if !r.Conclusion.Valid {
			continue
		}
		c := string(r.Conclusion.CheckConclusion)
		idx, ok := rank[c]
		if !ok {
			continue
		}
		if bestIdx < 0 || idx < bestIdx {
			bestIdx = idx
		}
	}
	if bestIdx < 0 {
		return "completed", "neutral"
	}
	return "completed", priority[bestIdx]
}
