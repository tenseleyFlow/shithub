// SPDX-License-Identifier: AGPL-3.0-or-later

// Package review owns PR-review orchestration: inline comments,
// review submission with attached pending comments, dismissal,
// reviewer requests, thread resolution, and the required-reviews
// gate consulted by the merge handler.
//
// Diff-position anchoring model (matches GitHub's contract): each
// comment captures (file_path, side, original_commit_sha,
// original_line, original_position). The PR synchronize pipeline
// re-walks the diff against the new head and updates each comment's
// current_position; comments whose anchor line no longer exists go
// to current_position=NULL ("outdated"). The position mapping
// helper here is invoked from `pulls.Synchronize`.
package review

import (
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps wires this package into the runtime.
type Deps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// Errors surfaced to handlers.
var (
	ErrEmptyBody              = errors.New("review: comment body is required")
	ErrBodyTooLong            = errors.New("review: body too long")
	ErrAuthorCannotApprove    = errors.New("review: author cannot approve their own PR")
	ErrInvalidState           = errors.New("review: state must be comment, approve, or request_changes")
	ErrCommentNotOnPR         = errors.New("review: comment does not belong to this PR")
	ErrAlreadyResolved        = errors.New("review: thread already resolved")
	ErrNotResolved            = errors.New("review: thread is not resolved")
	ErrReviewerLimitReached   = errors.New("review: 20 reviewers max per PR")
	ErrReviewerAlreadyPending = errors.New("review: reviewer already requested")
	ErrReviewNotFound         = errors.New("review: review not found")
)

// MaxReviewersPerPR caps active review requests per PR. Matches the
// spec pitfall section.
const MaxReviewersPerPR = 20
