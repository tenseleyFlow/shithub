// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	mdrender "github.com/tenseleyFlow/shithub/internal/repos/markdown"
)

// SubmitParams describes the submit-a-review action.
type SubmitParams struct {
	PRIssueID    int64
	AuthorUserID int64
	State        string // "comment" | "approve" | "request_changes"
	Body         string
	// PRAuthorUserID is the issues.author_user_id of the PR — used to
	// reject self-approval. Caller passes it in to keep this package
	// from cross-importing the pulls orchestrator.
	PRAuthorUserID int64
}

// Submit records the review row, attaches every pending draft comment
// the author has on this PR to the new review (one tx), and emits a
// `reviewed` timeline event with the state. Pending review-request
// rows owned by the author are satisfied when state is approve or
// request_changes.
func Submit(ctx context.Context, deps Deps, p SubmitParams) (pullsdb.PrReview, error) {
	if p.State != "comment" && p.State != "approve" && p.State != "request_changes" {
		return pullsdb.PrReview{}, ErrInvalidState
	}
	if p.State == "approve" && p.AuthorUserID != 0 && p.AuthorUserID == p.PRAuthorUserID {
		return pullsdb.PrReview{}, ErrAuthorCannotApprove
	}
	body := strings.TrimSpace(p.Body)
	if len(body) > 65535 {
		return pullsdb.PrReview{}, ErrBodyTooLong
	}
	html, _ := mdrender.RenderHTML([]byte(body))

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return pullsdb.PrReview{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := pullsdb.New()
	row, err := q.CreatePRReview(ctx, tx, pullsdb.CreatePRReviewParams{
		PrIssueID:      p.PRIssueID,
		AuthorUserID:   pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
		State:          pullsdb.PrReviewState(p.State),
		Body:           body,
		BodyHtmlCached: pgtype.Text{String: html, Valid: html != ""},
	})
	if err != nil {
		return pullsdb.PrReview{}, fmt.Errorf("insert review: %w", err)
	}

	// Attach every pending draft comment authored by this user on this
	// PR to the new review.
	if err := q.AttachPendingCommentsToReview(ctx, tx, pullsdb.AttachPendingCommentsToReviewParams{
		PrIssueID:    p.PRIssueID,
		AuthorUserID: pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
		ReviewID:     pgtype.Int8{Int64: row.ID, Valid: true},
	}); err != nil {
		return pullsdb.PrReview{}, fmt.Errorf("attach pending: %w", err)
	}

	// Satisfy any pending review request from the author (only on
	// approve / request_changes per spec).
	if p.State == "approve" || p.State == "request_changes" {
		if err := q.SatisfyPRReviewRequest(ctx, tx, pullsdb.SatisfyPRReviewRequestParams{
			PrIssueID:           p.PRIssueID,
			SatisfiedByReviewID: pgtype.Int8{Int64: row.ID, Valid: true},
			RequestedUserID:     pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
		}); err != nil {
			return pullsdb.PrReview{}, fmt.Errorf("satisfy request: %w", err)
		}
	}

	// `reviewed` timeline event.
	iq := issuesdb.New()
	if _, err := iq.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     p.PRIssueID,
		ActorUserID: pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
		Kind:        "reviewed",
		Meta:        []byte(fmt.Sprintf(`{"state":%q,"review_id":%d}`, p.State, row.ID)),
	}); err != nil {
		return pullsdb.PrReview{}, fmt.Errorf("emit event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return pullsdb.PrReview{}, err
	}
	committed = true
	return row, nil
}

// Dismiss flips a review's dismissed_at + reason. Caller has already
// gated on policy (typically repo admin only).
func Dismiss(ctx context.Context, deps Deps, actorUserID, reviewID int64, reason string) error {
	q := pullsdb.New()
	if _, err := q.GetPRReviewByID(ctx, deps.Pool, reviewID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReviewNotFound
		}
		return err
	}
	return q.DismissPRReview(ctx, deps.Pool, pullsdb.DismissPRReviewParams{
		ID:                 reviewID,
		DismissedByUserID:  pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		DismissalReason:    strings.TrimSpace(reason),
	})
}
