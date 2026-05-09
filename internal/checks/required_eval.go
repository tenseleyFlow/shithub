// SPDX-License-Identifier: AGPL-3.0-or-later

package checks

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
)

// GateInputs is the small struct the merge gate cares about.
type GateInputs struct {
	RepoID        int64
	HeadSHA       string
	RequiredNames []string // from branch_protection_rules.status_checks_required
}

// GateResult mirrors the spec's blocked-with-reason vocabulary.
type GateResult struct {
	Satisfied bool
	Reason    string // "" when Satisfied; otherwise human-readable cause
	// Missing is the subset of RequiredNames that haven't yet succeeded
	// or returned neutral on HeadSHA. Populated for UI tooltips.
	Missing []string
}

// EvaluateRequiredChecks returns Satisfied=true when every required
// check name has a `success` or `neutral` conclusion on HeadSHA. A
// missing run, an in-progress run, or any other conclusion (failure /
// stale / cancelled / etc.) is unsatisfied.
//
// The evaluator is the same one consulted by `pulls.Mergeability` and
// `pulls.Merge` — composes with `review.Evaluate` at the call site.
func EvaluateRequiredChecks(ctx context.Context, pool *pgxpool.Pool, in GateInputs) (GateResult, error) {
	if len(in.RequiredNames) == 0 {
		return GateResult{Satisfied: true}, nil
	}
	if in.HeadSHA == "" {
		// No head snapshot yet — block, since we can't verify any check
		// passed against an unknown commit.
		return GateResult{
			Reason:  "head SHA not yet recorded for this PR",
			Missing: append([]string(nil), in.RequiredNames...),
		}, nil
	}
	q := checksdb.New()
	missing := []string{}
	for _, name := range in.RequiredNames {
		run, err := q.GetLatestCheckRunByName(ctx, pool, checksdb.GetLatestCheckRunByNameParams{
			RepoID:  in.RepoID,
			HeadSha: in.HeadSHA,
			Name:    name,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				missing = append(missing, name)
				continue
			}
			return GateResult{}, fmt.Errorf("required-check lookup %q: %w", name, err)
		}
		if !runSatisfies(run) {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return GateResult{Satisfied: true}, nil
	}
	reason := "Required check missing: " + missing[0]
	if len(missing) > 1 {
		reason = fmt.Sprintf("Required checks missing: %v", missing)
	}
	return GateResult{Reason: reason, Missing: missing}, nil
}

// runSatisfies reports whether a single run "passes" for the
// required-check gate. Matches the spec: `success` and `neutral` on
// the head SHA satisfy; anything else (including in_progress) does
// not.
func runSatisfies(r checksdb.CheckRun) bool {
	if r.Status != checksdb.CheckStatusCompleted {
		return false
	}
	if !r.Conclusion.Valid {
		return false
	}
	switch r.Conclusion.CheckConclusion {
	case checksdb.CheckConclusionSuccess, checksdb.CheckConclusionNeutral:
		return true
	}
	return false
}
