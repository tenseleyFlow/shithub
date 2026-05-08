// SPDX-License-Identifier: AGPL-3.0-or-later

package review

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
)

// CommentParams describes a single inline comment on a PR. When
// `Pending` is true the comment is filed as a draft attached to the
// author's pending review (review_id stays NULL); SubmitReview later
// flips pending=false and sets review_id.
type CommentParams struct {
	PRIssueID         int64
	AuthorUserID      int64
	FilePath          string
	Side              string // "left" | "right"
	OriginalCommitSHA string
	OriginalLine      int32
	OriginalPosition  int32
	CurrentPosition   int32 // -1 means already outdated (rare on insert)
	Body              string
	InReplyToID       int64 // 0 means top-level
	Pending           bool
}

// AddComment inserts a single inline comment. When InReplyToID is set
// the new comment threads under that one (validated to belong to the
// same PR). Markdown is rendered to body_html_cached at insert time.
func AddComment(ctx context.Context, deps Deps, p CommentParams) (pullsdb.PrReviewComment, error) {
	body := strings.TrimSpace(p.Body)
	if body == "" {
		return pullsdb.PrReviewComment{}, ErrEmptyBody
	}
	if len(body) > 65535 {
		return pullsdb.PrReviewComment{}, ErrBodyTooLong
	}
	if p.Side != "left" && p.Side != "right" {
		p.Side = "right"
	}

	q := pullsdb.New()

	// Reply validation: parent must exist on the same PR. We don't
	// inherit FilePath/Side from the parent — the caller passes the
	// correct anchor; the in-reply-to link is purely for threading.
	if p.InReplyToID != 0 {
		parent, err := q.GetPRReviewComment(ctx, deps.Pool, p.InReplyToID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return pullsdb.PrReviewComment{}, ErrCommentNotOnPR
			}
			return pullsdb.PrReviewComment{}, err
		}
		if parent.PrIssueID != p.PRIssueID {
			return pullsdb.PrReviewComment{}, ErrCommentNotOnPR
		}
		// Inherit anchor from parent so the whole thread renders on
		// the same diff line even if the caller forgot to pass it.
		p.FilePath = parent.FilePath
		p.Side = string(parent.Side)
		p.OriginalCommitSHA = parent.OriginalCommitSha
		p.OriginalLine = parent.OriginalLine
		p.OriginalPosition = parent.OriginalPosition
		if parent.CurrentPosition.Valid {
			p.CurrentPosition = parent.CurrentPosition.Int32
		}
	}

	html, _ := mdrender.RenderHTML([]byte(body))

	cur := pgtype.Int4{}
	if p.CurrentPosition >= 0 {
		cur = pgtype.Int4{Int32: p.CurrentPosition, Valid: true}
	}

	row, err := q.CreatePRReviewComment(ctx, deps.Pool, pullsdb.CreatePRReviewCommentParams{
		PrIssueID:         p.PRIssueID,
		ReviewID:          pgtype.Int8{},
		AuthorUserID:      pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
		FilePath:          p.FilePath,
		Side:              pullsdb.PrReviewSide(p.Side),
		OriginalCommitSha: p.OriginalCommitSHA,
		OriginalLine:      p.OriginalLine,
		OriginalPosition:  p.OriginalPosition,
		CurrentPosition:   cur,
		Body:              body,
		BodyHtmlCached:    pgtype.Text{String: html, Valid: html != ""},
		InReplyToID:       pgtype.Int8{Int64: p.InReplyToID, Valid: p.InReplyToID != 0},
		Pending:           p.Pending,
	})
	if err != nil {
		return pullsdb.PrReviewComment{}, err
	}
	return row, nil
}

// EditComment updates the body of an existing comment. Caller is
// expected to have already enforced "actor == comment.author OR
// repo admin" via policy.
func EditComment(ctx context.Context, deps Deps, commentID int64, body string) error {
	body = strings.TrimSpace(body)
	if body == "" {
		return ErrEmptyBody
	}
	if len(body) > 65535 {
		return ErrBodyTooLong
	}
	html, _ := mdrender.RenderHTML([]byte(body))
	return pullsdb.New().UpdatePRReviewCommentBody(ctx, deps.Pool, pullsdb.UpdatePRReviewCommentBodyParams{
		ID: commentID, Body: body, BodyHtmlCached: pgtype.Text{String: html, Valid: html != ""},
	})
}

// Resolve marks the thread (root + replies) resolved. The "thread" is
// keyed off the comment id; UI walks `in_reply_to_id` to render
// replies, so resolving the root collapses the whole conversation.
// Default-collapsed in the Files tab; "Show resolved" toggle reopens.
func Resolve(ctx context.Context, deps Deps, actorUserID, commentID int64) error {
	q := pullsdb.New()
	c, err := q.GetPRReviewComment(ctx, deps.Pool, commentID)
	if err != nil {
		return err
	}
	if c.ResolvedAt.Valid {
		return ErrAlreadyResolved
	}
	return q.SetPRReviewCommentResolved(ctx, deps.Pool, pullsdb.SetPRReviewCommentResolvedParams{
		ID:               commentID,
		ResolvedAt:       pgtype.Timestamptz{Time: time.Now(), Valid: true},
		ResolvedByUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
	})
}

// Reopen clears the resolved-at marker on a thread.
func Reopen(ctx context.Context, deps Deps, commentID int64) error {
	q := pullsdb.New()
	c, err := q.GetPRReviewComment(ctx, deps.Pool, commentID)
	if err != nil {
		return err
	}
	if !c.ResolvedAt.Valid {
		return ErrNotResolved
	}
	return q.SetPRReviewCommentResolved(ctx, deps.Pool, pullsdb.SetPRReviewCommentResolvedParams{
		ID:               commentID,
		ResolvedAt:       pgtype.Timestamptz{Valid: false},
		ResolvedByUserID: pgtype.Int8{Valid: false},
	})
}
