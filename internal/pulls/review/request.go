// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/notif"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
)

// RequestParams describes a review-request action.
type RequestParams struct {
	PRIssueID         int64
	RequestedUserID   int64
	RequestedTeamID   int64
	RequestedByUserID int64
}

// Request creates a pr_review_requests row. Bounded by
// MaxReviewersPerPR per the spec pitfalls section.
func Request(ctx context.Context, deps Deps, p RequestParams) (pullsdb.PrReviewRequest, error) {
	if (p.RequestedUserID == 0) == (p.RequestedTeamID == 0) {
		return pullsdb.PrReviewRequest{}, ErrReviewerTargetRequired
	}
	q := pullsdb.New()
	count, err := q.CountActivePRReviewRequests(ctx, deps.Pool, p.PRIssueID)
	if err != nil {
		return pullsdb.PrReviewRequest{}, err
	}
	if int(count) >= MaxReviewersPerPR {
		return pullsdb.PrReviewRequest{}, ErrReviewerLimitReached
	}
	// Idempotent-ish: if the same reviewer is already pending, don't
	// add a duplicate row. Walking the active list is cheaper than
	// adding a partial unique index given v1 PRs typically have
	// single-digit reviewer counts.
	existing, err := q.ListPRReviewRequests(ctx, deps.Pool, p.PRIssueID)
	if err != nil {
		return pullsdb.PrReviewRequest{}, err
	}
	for _, e := range existing {
		active := !e.DismissedAt.Valid && !e.SatisfiedByReviewID.Valid
		if active && e.RequestedUserID.Valid && e.RequestedUserID.Int64 == p.RequestedUserID && p.RequestedUserID != 0 {
			return pullsdb.PrReviewRequest{}, ErrReviewerAlreadyPending
		}
		if active && e.RequestedTeamID.Valid && e.RequestedTeamID.Int64 == p.RequestedTeamID && p.RequestedTeamID != 0 {
			return pullsdb.PrReviewRequest{}, ErrReviewerAlreadyPending
		}
	}
	row, err := q.CreatePRReviewRequest(ctx, deps.Pool, pullsdb.CreatePRReviewRequestParams{
		PrIssueID:         p.PRIssueID,
		RequestedUserID:   pgtype.Int8{Int64: p.RequestedUserID, Valid: p.RequestedUserID != 0},
		RequestedTeamID:   pgtype.Int8{Int64: p.RequestedTeamID, Valid: p.RequestedTeamID != 0},
		RequestedByUserID: pgtype.Int8{Int64: p.RequestedByUserID, Valid: p.RequestedByUserID != 0},
	})
	if err != nil {
		return row, err
	}
	// S29: emit a domain event so the fan-out worker can route a
	// `review_requested` notification to the requested reviewer.
	// Best-effort — review-request side already succeeded; an emit
	// failure is logged but doesn't fail the request (the fan-out
	// worker isn't strict about ordering for this kind).
	issue, ierr := issuesdb.New().GetIssueByID(ctx, deps.Pool, p.PRIssueID)
	if ierr == nil && p.RequestedUserID != 0 {
		var public bool
		_ = deps.Pool.QueryRow(
			ctx,
			`SELECT visibility = 'public' FROM repos WHERE id = $1`,
			issue.RepoID,
		).Scan(&public)
		_ = notif.Emit(ctx, deps.Pool, notif.Event{
			ActorUserID: p.RequestedByUserID,
			Kind:        "review_requested",
			RepoID:      issue.RepoID,
			SourceKind:  "issue",
			SourceID:    issue.ID,
			Public:      public,
			Extra: map[string]any{
				"reviewer_user_id": p.RequestedUserID,
				"issue_number":     issue.Number,
				"issue_title":      issue.Title,
			},
		})
	}
	return row, nil
}
