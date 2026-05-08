// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
)

// RequestParams describes a review-request action.
type RequestParams struct {
	PRIssueID         int64
	RequestedUserID   int64
	RequestedByUserID int64
}

// Request creates a pr_review_requests row. Bounded by
// MaxReviewersPerPR per the spec pitfalls section.
func Request(ctx context.Context, deps Deps, p RequestParams) (pullsdb.PrReviewRequest, error) {
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
		if e.RequestedUserID.Valid && e.RequestedUserID.Int64 == p.RequestedUserID &&
			!e.DismissedAt.Valid && !e.SatisfiedByReviewID.Valid {
			return pullsdb.PrReviewRequest{}, ErrReviewerAlreadyPending
		}
	}
	return q.CreatePRReviewRequest(ctx, deps.Pool, pullsdb.CreatePRReviewRequestParams{
		PrIssueID:         p.PRIssueID,
		RequestedUserID:   pgtype.Int8{Int64: p.RequestedUserID, Valid: p.RequestedUserID != 0},
		RequestedTeamID:   pgtype.Int8{Valid: false},
		RequestedByUserID: pgtype.Int8{Int64: p.RequestedByUserID, Valid: p.RequestedByUserID != 0},
	})
}
