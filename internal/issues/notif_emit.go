// SPDX-License-Identifier: AGPL-3.0-or-later

package issues

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/markdown"
	"github.com/tenseleyFlow/shithub/internal/notif"
)

// emitIssueEventTx emits a domain event for an issue/PR mutation
// inside the orchestrator's tx. Centralized so every call site
// records the same shape (kind, source, repo, mentions).
func emitIssueEventTx(
	ctx context.Context,
	tx pgx.Tx,
	kind string,
	issue issuesdb.Issue,
	actorUserID int64,
	repoIsPublic bool,
	mentions []int64,
) error {
	return notif.EmitTx(ctx, tx, notif.Event{
		ActorUserID: actorUserID,
		Kind:        kind,
		RepoID:      issue.RepoID,
		SourceKind:  "issue",
		SourceID:    issue.ID,
		Public:      repoIsPublic,
		Mentions:    mentions,
		Extra: map[string]any{
			"issue_number": issue.Number,
			"issue_title":  issue.Title,
		},
	})
}

// emitCommentEventTx emits a comment-create event. Carries the
// underlying issue number/title so the inbox can render a useful
// summary without an extra join.
func emitCommentEventTx(
	ctx context.Context,
	tx pgx.Tx,
	kind string,
	issue issuesdb.Issue,
	commentID int64,
	actorUserID int64,
	repoIsPublic bool,
	mentions []int64,
) error {
	return notif.EmitTx(ctx, tx, notif.Event{
		ActorUserID: actorUserID,
		Kind:        kind,
		RepoID:      issue.RepoID,
		SourceKind:  "issue",
		SourceID:    issue.ID,
		Public:      repoIsPublic,
		Mentions:    mentions,
		Extra: map[string]any{
			"issue_number": issue.Number,
			"issue_title":  issue.Title,
			"comment_id":   strconv.FormatInt(commentID, 10),
		},
	})
}

// emitAssignmentEventTx fires when a user is added as an assignee.
// One event per assignee — the fan-out worker treats each as a
// separate "this user has been assigned" notification with reason
// `assignment`.
func emitAssignmentEventTx(
	ctx context.Context,
	tx pgx.Tx,
	issue issuesdb.Issue,
	actorUserID, assigneeUserID int64,
	repoIsPublic bool,
) error {
	return notif.EmitTx(ctx, tx, notif.Event{
		ActorUserID: actorUserID,
		Kind:        "issue_assigned",
		RepoID:      issue.RepoID,
		SourceKind:  "issue",
		SourceID:    issue.ID,
		Public:      repoIsPublic,
		// `assignee_user_id` lets the fan-out worker route the
		// notification to the assignee even if they're not yet a
		// thread subscriber.
		Extra: map[string]any{
			"issue_number":     issue.Number,
			"issue_title":      issue.Title,
			"assignee_user_id": assigneeUserID,
		},
	})
}

// repoVisibilityPublic resolves the repo's visibility (public/private)
// using the pool. Best-effort: errors collapse to false (private),
// which is the safe default for the public-feed flag on the event row.
func repoVisibilityPublic(ctx context.Context, db pgxRow, repoID int64) (bool, error) {
	var vis string
	err := db.QueryRow(ctx,
		`SELECT visibility::text FROM repos WHERE id = $1`,
		repoID,
	).Scan(&vis)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return vis == "public", nil
}

// pgxRow is the minimum surface emit helpers need from a tx or pool.
type pgxRow interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// mentionUserIDs is the call-site ergonomic wrapper around
// notif.ResolveMentionUserIDs. Lives here so issues.go's call site
// stays a one-liner.
func mentionUserIDs(ctx context.Context, pool *pgxpool.Pool, ms []markdown.Mention) []int64 {
	return notif.ResolveMentionUserIDs(ctx, pool, ms)
}
