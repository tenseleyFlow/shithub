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
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
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
	// Audit is optional; when non-nil, state-changing orchestrator
	// calls (SetState, SetLock, AddComment) record an audit row. The
	// repo lifecycle package writes audit rows directly via deps.Audit;
	// this field ensures issues/PR mutations are equally traceable
	// (S00-S25 audit, M).
	Audit *audit.Recorder
}

// Errors returned by the orchestrator. Handlers map these to status
// codes + friendly user-facing messages.
var (
	ErrEmptyTitle        = errors.New("issues: title is required")
	ErrTitleTooLong      = errors.New("issues: title too long (max 256)")
	ErrBodyTooLong       = errors.New("issues: body too long")
	ErrEmptyComment      = errors.New("issues: comment body is required")
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
	html, mentions := renderBody(ctx, deps, p.Body)
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

	// Emit the domain event in the same tx as the issue row so a
	// rollback drops both. Mention resolution happens *after* commit
	// to avoid holding the row lock through user-id lookups; the
	// fan-out worker reads payload.mentions to drive @-ping
	// recipients.
	mentionIDs := mentionUserIDs(ctx, deps.Pool, mentions)
	repoVis, _ := repoVisibilityPublic(ctx, tx, p.RepoID)
	eventKind := "issue_created"
	if kind == "pr" {
		eventKind = "pr_opened"
	}
	if err := emitIssueEventTx(ctx, tx, eventKind, row, p.AuthorUserID, repoVis, mentionIDs); err != nil {
		return issuesdb.Issue{}, fmt.Errorf("emit event: %w", err)
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
	if body == "" {
		return issuesdb.IssueComment{}, ErrEmptyComment
	}
	if len(body) > 65535 {
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

	html, mentions := renderBody(ctx, deps, body)

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

	mentionIDs := mentionUserIDs(ctx, deps.Pool, mentions)
	repoVis, _ := repoVisibilityPublic(ctx, tx, issue.RepoID)
	commentKind := "issue_comment_created"
	if issue.Kind == issuesdb.IssueKindPr {
		commentKind = "pr_comment_created"
	}
	if err := emitCommentEventTx(ctx, tx, commentKind, issue, c.ID, p.AuthorUserID, repoVis, mentionIDs); err != nil {
		return issuesdb.IssueComment{}, fmt.Errorf("emit event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return issuesdb.IssueComment{}, err
	}
	committed = true
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, p.AuthorUserID,
			audit.ActionIssueCommentCreated, audit.TargetIssue, p.IssueID,
			map[string]any{"comment_id": c.ID})
	}
	return c, nil
}

// SetState closes or reopens an issue and emits an `closed` /
// `reopened` event.
func SetState(ctx context.Context, deps Deps, actorUserID, issueID int64, newState, reason string) error {
	return setState(ctx, deps, actorUserID, issueID, newState, reason, 0)
}

// SetStateWithComment is used by the issue comment form when the submit
// button both creates a comment and changes state. The timeline event stores
// the comment id so the UI can keep the state badge adjacent to the comment
// that caused it.
func SetStateWithComment(ctx context.Context, deps Deps, actorUserID, issueID int64, newState, reason string, commentID int64) error {
	return setState(ctx, deps, actorUserID, issueID, newState, reason, commentID)
}

func setState(ctx context.Context, deps Deps, actorUserID, issueID int64, newState, reason string, commentID int64) error {
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
	meta := emptyMeta
	if commentID != 0 {
		meta = []byte(`{"comment_id":` + strconv.FormatInt(commentID, 10) + `}`)
	}
	if _, err := q.InsertIssueEvent(ctx, tx, issuesdb.InsertIssueEventParams{
		IssueID:     issueID,
		ActorUserID: pgtype.Int8{Int64: actorUserID, Valid: actorUserID != 0},
		Kind:        kind,
		Meta:        meta,
	}); err != nil {
		return err
	}
	// Lifecycle domain event so the notif fan-out + S33 webhook
	// pipeline pick up state changes the same way they pick up
	// comments.
	issue, _ := q.GetIssueByID(ctx, tx, issueID)
	repoVis, _ := repoVisibilityPublic(ctx, tx, issue.RepoID)
	stateKind := "issue_" + kind // issue_closed | issue_reopened
	if issue.Kind == issuesdb.IssueKindPr {
		stateKind = "pr_" + kind
	}
	if err := emitIssueEventTx(ctx, tx, stateKind, issue, actorUserID, repoVis, nil); err != nil {
		return fmt.Errorf("emit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	committed = true
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionIssueStateChanged, audit.TargetIssue, issueID,
			map[string]any{"state": newState, "reason": reason})
	}
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
	if deps.Audit != nil {
		_ = deps.Audit.Record(ctx, deps.Pool, actorUserID,
			audit.ActionIssueLockChanged, audit.TargetIssue, issueID,
			map[string]any{"locked": locked, "reason": reason})
	}
	return nil
}

// renderBody renders markdown to sanitized HTML and returns the
// resolved mention list. Body length is bounded upstream
// (orchestrator validation + DB CHECK at 65535), so
// ErrInputTooLarge is structurally impossible here — but if it ever
// fires, log loudly: it means a precondition somewhere upstream
// regressed. Mentions feed the S29 fan-out worker via the event
// payload's `mentions` array.
func renderBody(ctx context.Context, deps Deps, body string) (string, []mdrender.Mention) {
	if body == "" {
		return "", nil
	}
	html, _, mentions, err := mdrender.Render(ctx, []byte(body), mdrender.Options{
		SoftBreakAsBR: true,
	})
	if err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "issues: markdown render failed",
			"error", err, "body_bytes", len(body))
		return "", nil
	}
	return string(html), mentions
}
