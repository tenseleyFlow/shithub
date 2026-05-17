// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos/protection"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

// GateInputs is the small struct the merge gate cares about.
type GateInputs struct {
	RepoID    int64
	BaseRef   string
	BaseOID   string
	GitDir    string
	PRIssueID int64
}

// GateResult mirrors the spec's mergeable_state vocabulary for
// review-side blocks.
type GateResult struct {
	// Satisfied — the PR meets the review requirements (or none apply).
	Satisfied bool
	// Reason — short human-readable cause when not satisfied. Used by
	// the UI banner ("Approval required (1/1 reviews missing)").
	Reason string
	// RequiredCount — the rule's required_review_count.
	RequiredCount int
	// CurrentApprovals — count of "latest review per author" with state
	// approve. Always excludes the PR author.
	CurrentApprovals int
	// HasRequestChanges — at least one undismissed `request_changes`
	// review whose author hasn't superseded it with a later approve.
	HasRequestChanges bool
	// RequiresCodeOwnerReview — the matching branch-protection rule
	// requires code-owner approval for owned paths.
	RequiresCodeOwnerReview bool
	// CodeOwnerReviewSatisfied — true when every owned changed file has
	// at least one valid code-owner approval.
	CodeOwnerReviewSatisfied bool
	// MissingCodeOwnerPaths — changed paths still waiting on a valid
	// code-owner approval, or owned by unresolved CODEOWNERS entries.
	MissingCodeOwnerPaths []string
}

// Evaluate computes the gate against the matching branch-protection
// rule. Returns Satisfied=true when no rule matches the base ref or
// when the rule's required_review_count == 0 and no unresolved
// request_changes reviews exist.
//
// "Latest review per author" semantics: when an author submits a
// request_changes and later submits an approve, only the approve
// counts. When two distinct authors approve and one of them later
// submits a comment, both still count for the approval tally — only
// approve and request_changes shift the per-author state.
func Evaluate(ctx context.Context, pool *pgxpool.Pool, in GateInputs, prAuthorUserID int64) (GateResult, error) {
	// Locate the longest-pattern-matching protection rule for this
	// base ref. The same matching helper that S20 uses lives in
	// internal/repos/protection but we don't import it here to avoid
	// a circular dependency on the repos package — duplicate the
	// `filepath.Match` longest-match shape inline (cheap).
	rules, err := loadProtectionRules(ctx, pool, in.RepoID)
	if err != nil {
		return GateResult{}, err
	}
	rule, hasRule := protection.MatchLongestRule(rules, in.BaseRef)

	// If there's no rule, approve count + request_changes still gate
	// the merge per spec ("no unresolved request_changes reviews").
	q := pullsdb.New()
	reviews, err := q.ListUndismissedReviewsForGate(ctx, pool, in.PRIssueID)
	if err != nil {
		return GateResult{}, err
	}
	latest := latestPerAuthor(reviews, prAuthorUserID)
	approves, requestChanges := countLatestDecisions(latest)

	required := 0
	if hasRule {
		required = int(rule.RequiredReviewCount)
	}
	out := GateResult{
		RequiredCount:     required,
		CurrentApprovals:  approves,
		HasRequestChanges: requestChanges,
	}
	if hasRule && rule.RequireCodeOwnerReview {
		out.RequiresCodeOwnerReview = true
		repo, err := reposdb.New().GetRepoByID(ctx, pool, in.RepoID)
		if err != nil {
			return GateResult{}, err
		}
		approved := latestApprovers(latest)
		ok, missing, err := codeOwnerReviewSatisfied(ctx, pool, in, repo, approved)
		if err != nil {
			return GateResult{}, err
		}
		out.CodeOwnerReviewSatisfied = ok
		out.MissingCodeOwnerPaths = missing
	}

	switch {
	case requestChanges:
		out.Reason = "Changes requested by a reviewer."
	case approves < required:
		out.Reason = "Approval required."
	case out.RequiresCodeOwnerReview && !out.CodeOwnerReviewSatisfied:
		out.Reason = "Code owner review required."
	default:
		out.Satisfied = true
	}
	return out, nil
}

// loadProtectionRules reads every rule on the repo. Tiny set in
// practice; cheaper than per-request matching at the SQL layer.
func loadProtectionRules(ctx context.Context, pool *pgxpool.Pool, repoID int64) ([]reposdb.BranchProtectionRule, error) {
	return reposdb.New().ListBranchProtectionRules(ctx, pool, repoID)
}

// matchRule duplicates the longest-pattern-wins algorithm from
// (matchRule extracted to protection.MatchLongestRule — see audit fix.)

// latestPerAuthor reduces reviews to a per-author "winning" state.
// approve and request_changes update the author's tally; comment-state
// reviews are ignored (they don't shift the gate state per spec).
//
// Author of the PR is excluded from the approval count.
func latestPerAuthor(reviews []pullsdb.ListUndismissedReviewsForGateRow, prAuthorUserID int64) map[int64]pullsdb.PrReviewState {
	out := map[int64]pullsdb.PrReviewState{}
	// Reviews are ordered by (author_user_id, submitted_at) so the
	// last entry per author is the latest decision. Track the per-
	// author winner inline.
	var (
		curAuthor int64 = 0
		curState  pullsdb.PrReviewState
		curValid  bool
	)
	flush := func() {
		if !curValid || curAuthor == prAuthorUserID || curAuthor == 0 {
			return
		}
		switch curState {
		case pullsdb.PrReviewStateApprove, pullsdb.PrReviewStateRequestChanges:
			out[curAuthor] = curState
		}
	}
	for _, r := range reviews {
		auth := int64FromPg(r.AuthorUserID)
		if auth != curAuthor {
			flush()
			curAuthor = auth
			curState = r.State
			curValid = true
			continue
		}
		// Same author — newer row wins, but only approve / request_changes
		// shift the author's state. A trailing `comment` doesn't reset
		// the prior `approve`.
		switch r.State {
		case pullsdb.PrReviewStateApprove, pullsdb.PrReviewStateRequestChanges:
			curState = r.State
		}
	}
	flush()
	return out
}

func countLatestDecisions(latest map[int64]pullsdb.PrReviewState) (approves int, requestChanges bool) {
	for _, state := range latest {
		switch state {
		case pullsdb.PrReviewStateApprove:
			approves++
		case pullsdb.PrReviewStateRequestChanges:
			requestChanges = true
		}
	}
	return approves, requestChanges
}

func latestApprovers(latest map[int64]pullsdb.PrReviewState) map[int64]struct{} {
	out := map[int64]struct{}{}
	for authorID, state := range latest {
		if state == pullsdb.PrReviewStateApprove {
			out[authorID] = struct{}{}
		}
	}
	return out
}

func int64FromPg(p pgtype.Int8) int64 {
	if !p.Valid {
		return 0
	}
	return p.Int64
}
