// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/issues"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// ScheduledIssueCreateDeps wires the scheduled-issue worker. issues.Deps
// holds the shared orchestrator state (pool + limiter + logger + audit);
// the dedicated Pool field is retained because we read/update the
// user_scheduled_issues row outside any issue-creation transaction.
type ScheduledIssueCreateDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Audit  *audit.Recorder
	// IssuesLimiter is the same comment-throttle the live new-issue
	// handler uses. Reused so a runaway scheduling script can't burst
	// past the shared rate budget.
	IssuesLimiter *throttle.Limiter
}

// ScheduledIssueCreatePayload — id of the user_scheduled_issues row.
type ScheduledIssueCreatePayload struct {
	ScheduledID int64 `json:"scheduled_id"`
}

// ScheduledIssueCreate materializes a queued user_scheduled_issues row
// into a real issue. Idempotent on status: a retry after the issue has
// been created (or after the user cancelled the schedule) is a no-op
// rather than a duplicate. Permanent errors (cancelled row, missing
// repo) flip status to 'failed' with a reason and return a poison
// error so the worker doesn't retry; transient errors bubble up.
func ScheduledIssueCreate(deps ScheduledIssueCreateDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p ScheduledIssueCreatePayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.ScheduledID == 0 {
			return worker.PoisonError(errors.New("missing scheduled_id"))
		}
		uq := usersdb.New()
		row, err := uq.GetScheduledIssueByID(ctx, deps.Pool, p.ScheduledID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// User-deleted (CASCADE on users) — silently complete.
				return nil
			}
			return err
		}
		switch row.Status {
		case usersdb.ScheduledIssueStatusCancelled:
			deps.Logger.InfoContext(ctx, "scheduled-issue: cancelled at job time",
				"scheduled_id", p.ScheduledID, "user_id", row.UserID, "repo_id", row.RepoID)
			return nil
		case usersdb.ScheduledIssueStatusCreated:
			// Idempotency: a retry after a successful run is a no-op.
			return nil
		case usersdb.ScheduledIssueStatusFailed:
			// Don't keep retrying a poisoned row.
			return nil
		}

		created, err := issues.Create(ctx, issues.Deps{
			Pool:    deps.Pool,
			Logger:  deps.Logger,
			Audit:   deps.Audit,
			Limiter: deps.IssuesLimiter,
		}, issues.CreateParams{
			RepoID:       row.RepoID,
			AuthorUserID: row.UserID,
			Title:        row.Title,
			Body:         row.Body,
			Kind:         "issue",
		})
		if err != nil {
			// issues.Create's input-shape errors (ErrEmptyTitle, etc.)
			// were already enforced at handler time, so any error here
			// is operational (DB, repo not found). Mark failed so the
			// settings UI can show the reason; poison so the worker
			// doesn't retry an issue that may have side effects.
			if markErr := uq.MarkScheduledIssueFailed(ctx, deps.Pool, usersdb.MarkScheduledIssueFailedParams{
				ID:            p.ScheduledID,
				FailureReason: pgtype.Text{String: err.Error(), Valid: true},
			}); markErr != nil {
				deps.Logger.WarnContext(ctx, "scheduled-issue: mark failed", "error", markErr, "scheduled_id", p.ScheduledID)
			}
			return worker.PoisonError(fmt.Errorf("issues.Create: %w", err))
		}

		if err := uq.MarkScheduledIssueCreated(ctx, deps.Pool, usersdb.MarkScheduledIssueCreatedParams{
			ID:             p.ScheduledID,
			CreatedIssueID: pgtype.Int8{Int64: created.ID, Valid: true},
		}); err != nil {
			// The issue is created in the DB — losing the back-pointer
			// is non-fatal but worth logging. Don't poison; retry will
			// land on the no-op above because status is 'created' once
			// the next attempt sees the back-pointer (or never; we
			// accept the small window).
			deps.Logger.WarnContext(ctx, "scheduled-issue: mark created", "error", err,
				"scheduled_id", p.ScheduledID, "issue_id", created.ID)
		}
		return nil
	}
}
