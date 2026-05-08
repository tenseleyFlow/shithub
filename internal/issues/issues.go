// SPDX-License-Identifier: AGPL-3.0-or-later

// Package issues owns the issue + comment + label + milestone domain
// logic. Web handlers call into this package; the package owns
// transactions, cross-reference parsing, and event emission.
//
// PR-specific behavior lives in the future internal/pulls/ package
// (S22) — but PRs reuse the `issues` and `issue_comments` tables, so
// the queries here are kind-discriminated to keep the surface clean.
package issues

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
)

// Deps wires this package against the rest of the runtime. Pool is
// required; Limiter governs the comment-create rate limit; Logger is
// optional (falls back to discarding when nil).
type Deps struct {
	Pool    *pgxpool.Pool
	Limiter *throttle.Limiter
	Logger  *slog.Logger
}

// Errors returned by the orchestrator. Handlers map these to status
// codes + friendly user-facing messages.
var (
	ErrEmptyTitle        = errors.New("issues: title is required")
	ErrTitleTooLong      = errors.New("issues: title too long (max 256)")
	ErrBodyTooLong       = errors.New("issues: body too long")
	ErrCommentTooLong    = errors.New("issues: comment too long")
	ErrIssueLocked       = errors.New("issues: issue is locked")
	ErrCommentRateLimit  = errors.New("issues: comment rate limit exceeded")
	ErrLabelExists       = errors.New("issues: label name already taken on this repo")
	ErrLabelInvalidColor = errors.New("issues: label color must be 6 hex chars")
	ErrMilestoneExists   = errors.New("issues: milestone title already taken on this repo")
	ErrIssueNotFound     = errors.New("issues: issue not found")
)

// CreateParams describes a new-issue request.
type CreateParams struct {
	RepoID       int64
	AuthorUserID int64 // 0 means anonymous (unsupported in S21; handler enforces)
	Title        string
	Body         string
	// Kind defaults to "issue"; PR creation in S22 passes "pr".
	Kind string
}

// Create validates inputs, allocates a per-repo number atomically,
// inserts the row, renders the body's markdown, and returns the
// fresh issue. Default labels live with repo create; per-issue
// label/assignee/milestone application is a separate call.
//
// Cross-reference indexing fires asynchronously inside the same tx
// via insertReferencesFromBody — refs to other issues create
// `issue_references` rows + a `referenced` event on the target.
func Create(ctx context.Context, deps Deps, p CreateParams) (issuesdb.Issue, error) {
	title := strings.TrimSpace(p.Title)
	if title == "" {
		return issuesdb.Issue{}, ErrEmptyTitle
	}
	if len(title) > 256 {
		return issuesdb.Issue{}, ErrTitleTooLong
	}
	if len(p.Body) > 65535 {
		return issuesdb.Issue{}, ErrBodyTooLong
	}
	kind := p.Kind
	if kind == "" {
		kind = "issue"
	}

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return issuesdb.Issue{}, fmt.Errorf("begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := issuesdb.New()
	if err := q.EnsureRepoIssueCounter(ctx, tx, p.RepoID); err != nil {
		return issuesdb.Issue{}, fmt.Errorf("counter init: %w", err)
	}
	num, err := q.AllocateIssueNumber(ctx, tx, p.RepoID)
	if err != nil {
		return issuesdb.Issue{}, fmt.Errorf("allocate number: %w", err)
	}

	row, err := q.CreateIssue(ctx, tx, issuesdb.CreateIssueParams{
		RepoID:       p.RepoID,
		Number:       num,
		Kind:         issuesdb.IssueKind(kind),
		Title:        title,
		Body:         p.Body,
		AuthorUserID: pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
	})
	if err != nil {
		return issuesdb.Issue{}, fmt.Errorf("insert: %w", err)
	}

	// Render markdown for the cached body html.
	html, _ := mdrender.RenderHTML([]byte(p.Body))
	row.BodyHtmlCached = pgtype.Text{String: html, Valid: html != ""}

	if err := q.UpdateIssueTitleBody(ctx, tx, issuesdb.UpdateIssueTitleBodyParams{
		ID: row.ID, Title: row.Title, Body: row.Body,
		BodyHtmlCached: pgtype.Text{String: html, Valid: html != ""},
	}); err != nil {
		return issuesdb.Issue{}, fmt.Errorf("update html: %w", err)
	}

	if err := insertReferencesFromBody(ctx, tx, deps, row, p.Body, "issue_body", row.ID); err != nil {
		return issuesdb.Issue{}, fmt.Errorf("refs: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return issuesdb.Issue{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return row, nil
}

// CommentCreateParams is the input for AddComment.
type CommentCreateParams struct {
	IssueID      int64
	AuthorUserID int64
	Body         string
	// IsCollab signals that the actor's policy.Can role is at least
	// triage. Used to bypass the locked-issue gate.
	IsCollab bool
}

// AddComment validates, applies the rate limit, inserts the comment,
// and indexes references. Returns the fresh comment.
func AddComment(ctx context.Context, deps Deps, p CommentCreateParams) (issuesdb.IssueComment, error) {
	body := strings.TrimSpace(p.Body)
	if body == "" || len(body) > 65535 {
		return issuesdb.IssueComment{}, ErrCommentTooLong
	}
	if deps.Limiter != nil && p.AuthorUserID != 0 {
		if err := deps.Limiter.Hit(ctx, deps.Pool, throttle.Limit{
			Scope:      "issue_comment",
			Identifier: fmt.Sprintf("user:%d", p.AuthorUserID),
			Max:        20,
			Window:     time.Hour,
		}); err != nil {
			return issuesdb.IssueComment{}, ErrCommentRateLimit
		}
	}

	q := issuesdb.New()
	issue, err := q.GetIssueByID(ctx, deps.Pool, p.IssueID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return issuesdb.IssueComment{}, ErrIssueNotFound
		}
		return issuesdb.IssueComment{}, err
	}
	if issue.Locked && !p.IsCollab {
		return issuesdb.IssueComment{}, ErrIssueLocked
	}

	html, _ := mdrender.RenderHTML([]byte(body))

	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return issuesdb.IssueComment{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	c, err := q.CreateIssueComment(ctx, tx, issuesdb.CreateIssueCommentParams{
		IssueID:        p.IssueID,
		AuthorUserID:   pgtype.Int8{Int64: p.AuthorUserID, Valid: p.AuthorUserID != 0},
		Body:           body,
		BodyHtmlCached: pgtype.Text{String: html, Valid: html != ""},
	})
	if err != nil {
		return issuesdb.IssueComment{}, err
	}

	if err := insertReferencesFromBody(ctx, tx, deps, issue, body, "comment_body", c.ID); err != nil {
		return issuesdb.IssueComment{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return issuesdb.IssueComment{}, err
	}
	committed = true
	return c, nil
}

// SetState closes or reopens an issue and emits an `closed` /
// `reopened` event.
func SetState(ctx context.Context, deps Deps, actorUserID, issueID int64, newState, reason string) error {
	if newState != "open" && newState != "closed" {
		return errors.New("issues: state must be open or closed")
	}
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	q := issuesdb.New()
	closedBy := pgtype.Int8{}
	if newState == "closed" && actorUserID != 0 {
		closedBy = pgtype.Int8{Int64: actorUserID, Valid: true}
	}
	stateReason := pgtype.Text{}
	if reason != "" {
		stateReason = pgtype.Text{String: reason, Valid: true}
	}
	if err := q.SetIssueState(ctx, tx, issuesdb.SetIssueStateParams{
		ID:             issueID,
		State:          issuesdb.IssueState(newState),
		StateReason:    issuesdb.NullIssueStateReason{IssueStateReason: issuesdb.IssueStateReason(stateReason.String), Valid: stateReason.Valid},
		ClosedByUserID: closedBy,
	}); err != nil {
		return err
	}
	kind := "closed"
	if newState == "open" {
		kind = "reopened"
	}
	if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     issueID,
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind:        kind,
		Meta:        emptyMeta,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}

// emptyMeta is the JSON object passed when an event carries no metadata.
// The column is NOT NULL DEFAULT '{}'::jsonb, but binding nil from Go
// sends SQL NULL rather than letting the DEFAULT fire — so callers
// pass this explicitly.
var emptyMeta = []byte("{}")

// SetLock toggles the locked flag and emits an event.
func SetLock(ctx context.Context, deps Deps, actorUserID, issueID int64, locked bool, reason string) error {
	q := issuesdb.New()
	tx, err := deps.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()
	rsn := pgtype.Text{}
	if reason != "" {
		rsn = pgtype.Text{String: reason, Valid: true}
	}
	if err := q.SetIssueLock(ctx, tx, issuesdb.SetIssueLockParams{
		ID: issueID, Locked: locked, LockReason: rsn,
	}); err != nil {
		return err
	}
	kind := "locked"
	if !locked {
		kind = "unlocked"
	}
	if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID: issueID, ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind: kind,
		Meta: emptyMeta,
	}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	return nil
}
