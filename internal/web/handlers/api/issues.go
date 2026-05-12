// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountIssues registers the S50 §3 issue REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/issues                     list
//	POST   /api/v1/repos/{o}/{r}/issues                     create
//	GET    /api/v1/repos/{o}/{r}/issues/{number}            get
//	PATCH  /api/v1/repos/{o}/{r}/issues/{number}            update (title, body, state, state_reason)
//	GET    /api/v1/repos/{o}/{r}/issues/{number}/comments   list comments
//	POST   /api/v1/repos/{o}/{r}/issues/{number}/comments   add comment
//	PATCH  /api/v1/repos/{o}/{r}/issues/comments/{id}       edit comment
//	DELETE /api/v1/repos/{o}/{r}/issues/comments/{id}       delete comment
//	PUT    /api/v1/repos/{o}/{r}/issues/{number}/lock       lock
//	DELETE /api/v1/repos/{o}/{r}/issues/{number}/lock       unlock
//
// PAT scopes: repo:read on GETs, repo:write on mutations. Policy gates
// (ActionIssueRead/Create/Close/etc.) layer on top of the scope check;
// existence-leak-safe 404 on visibility miss.
func (h *Handlers) mountIssues(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/issues", h.issuesList)
		r.Get("/api/v1/repos/{owner}/{repo}/issues/{number}", h.issueGet)
		r.Get("/api/v1/repos/{owner}/{repo}/issues/{number}/comments", h.issueCommentsList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/issues", h.issueCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/issues/{number}", h.issuePatch)
		r.Post("/api/v1/repos/{owner}/{repo}/issues/{number}/comments", h.issueCommentCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/issues/comments/{cid}", h.issueCommentUpdate)
		r.Delete("/api/v1/repos/{owner}/{repo}/issues/comments/{cid}", h.issueCommentDelete)
		r.Put("/api/v1/repos/{owner}/{repo}/issues/{number}/lock", h.issueLock)
		r.Delete("/api/v1/repos/{owner}/{repo}/issues/{number}/lock", h.issueUnlock)
	})
}

// ─── presentation ───────────────────────────────────────────────────

type issueResponse struct {
	ID          int64    `json:"id"`
	Number      int64    `json:"number"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason,omitempty"`
	Locked      bool     `json:"locked"`
	LockReason  string   `json:"lock_reason,omitempty"`
	AuthorID    int64    `json:"author_id,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	ClosedAt    string   `json:"closed_at,omitempty"`
}

func presentIssue(i issuesdb.Issue, labels []string) issueResponse {
	out := issueResponse{
		ID:        i.ID,
		Number:    i.Number,
		Title:     i.Title,
		Body:      i.Body,
		State:     string(i.State),
		Locked:    i.Locked,
		Labels:    labels,
		CreatedAt: i.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt: i.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if i.StateReason.Valid {
		out.StateReason = string(i.StateReason.IssueStateReason)
	}
	if i.LockReason.Valid {
		out.LockReason = i.LockReason.String
	}
	if i.AuthorUserID.Valid {
		out.AuthorID = i.AuthorUserID.Int64
	}
	if i.ClosedAt.Valid {
		out.ClosedAt = i.ClosedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

type commentResponse struct {
	ID        int64  `json:"id"`
	IssueID   int64  `json:"issue_id"`
	AuthorID  int64  `json:"author_id,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	EditedAt  string `json:"edited_at,omitempty"`
}

func presentComment(c issuesdb.IssueComment) commentResponse {
	out := commentResponse{
		ID:        c.ID,
		IssueID:   c.IssueID,
		Body:      c.Body,
		CreatedAt: c.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt: c.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if c.AuthorUserID.Valid {
		out.AuthorID = c.AuthorUserID.Int64
	}
	if c.EditedAt.Valid {
		out.EditedAt = c.EditedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

// ─── list ───────────────────────────────────────────────────────────

func (h *Handlers) issuesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	stateFilter := normalizeIssueState(r.URL.Query().Get("state"))
	q := issuesdb.New()
	total, err := q.CountIssues(r.Context(), h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      repo.ID,
		StateFilter: stateFilter,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count issues", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListIssues(r.Context(), h.d.Pool, issuesdb.ListIssuesParams{
		RepoID:      repo.ID,
		Limit:       int32(perPage),
		Offset:      int32((page - 1) * perPage),
		StateFilter: stateFilter,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list issues", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	out := make([]issueResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentIssue(row, h.labelNamesFor(r.Context(), row.ID)))
	}
	writeJSON(w, http.StatusOK, out)
}

func normalizeIssueState(s string) pgtype.Text {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "open":
		return pgtype.Text{String: "open", Valid: true}
	case "closed":
		return pgtype.Text{String: "closed", Valid: true}
	case "", "all":
		// Encoded as NULL in the sqlc query so the WHERE clause is a no-op.
		return pgtype.Text{}
	default:
		// Unknown values fall back to "all" — gh-style leniency for
		// list endpoints; tightening would break script ports.
		return pgtype.Text{}
	}
}

func (h *Handlers) labelNamesFor(ctx httpRequestCtx, issueID int64) []string {
	rows, err := issuesdb.New().ListLabelsOnIssue(ctx, h.d.Pool, issueID)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out
}

// httpRequestCtx is a tiny alias used only as a parameter type so the
// labelNamesFor signature reads naturally (we don't want to import net.
// or context in this file just for that). We rely on Go assigning the
// request context to context.Context via the implicit interface.
type httpRequestCtx = ctxLike

type ctxLike interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}

// ─── single get ─────────────────────────────────────────────────────

func (h *Handlers) issueGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	num, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return
	}
	issue, err := issuesdb.New().GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: repo.ID, Number: num,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "issue not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get issue", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if issue.Kind != issuesdb.IssueKindIssue {
		// PRs share the `issues` table but are not exposed on the
		// /issues REST surface — they get their own routes in §4.
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return
	}
	writeJSON(w, http.StatusOK, presentIssue(issue, h.labelNamesFor(r.Context(), issue.ID)))
}

// ─── create ─────────────────────────────────────────────────────────

type issueCreateRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

func (h *Handlers) issueCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueCreate)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	var body issueCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	issue, err := issues.Create(r.Context(), h.issuesDeps(), issues.CreateParams{
		RepoID:       repo.ID,
		AuthorUserID: auth.UserID,
		Title:        body.Title,
		Body:         body.Body,
		Kind:         "issue",
	})
	if err != nil {
		writeIssuesError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentIssue(issue, nil))
}

// ─── patch ──────────────────────────────────────────────────────────

type issuePatchRequest struct {
	Title       *string `json:"title,omitempty"`
	Body        *string `json:"body,omitempty"`
	State       *string `json:"state,omitempty"`
	StateReason *string `json:"state_reason,omitempty"`
}

func (h *Handlers) issuePatch(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	num, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return
	}
	q := issuesdb.New()
	issue, err := q.GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: repo.ID, Number: num,
	})
	if err != nil || issue.Kind != issuesdb.IssueKindIssue {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return
	}
	var body issuePatchRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Title/body: author OR repo collaborator with at least
	// triage-equivalent permissions can edit. We gate via
	// ActionIssueComment since it matches the "trusted contributor"
	// archetype (comment + edit-own-issue privileges).
	if body.Title != nil || body.Body != nil {
		// Only the author (or someone with comment-equivalent
		// privileges on the repo) edits.
		canEdit := false
		if issue.AuthorUserID.Valid && issue.AuthorUserID.Int64 == auth.UserID {
			canEdit = true
		}
		if !canEdit {
			canEdit = policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionIssueComment, policy.NewRepoRefFromRepo(*repo)).Allow
		}
		if !canEdit {
			writeAPIError(w, http.StatusForbidden, "only the author or a collaborator may edit this issue")
			return
		}
		updated, err := issues.Edit(r.Context(), h.issuesDeps(), issues.EditParams{
			IssueID: issue.ID,
			Title:   body.Title,
			Body:    body.Body,
		})
		if err != nil {
			writeIssuesError(w, err)
			return
		}
		issue = updated
	}

	if body.State != nil {
		// State changes require ActionIssueClose.
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionIssueClose, policy.NewRepoRefFromRepo(*repo)).Allow {
			writeAPIError(w, http.StatusForbidden, "lack permission to change issue state")
			return
		}
		newState := strings.ToLower(*body.State)
		if newState != "open" && newState != "closed" {
			writeAPIError(w, http.StatusUnprocessableEntity, "state must be open or closed")
			return
		}
		reason := ""
		if body.StateReason != nil {
			reason = strings.ToLower(*body.StateReason)
			switch reason {
			case "", "completed", "not_planned", "duplicate", "reopened":
			default:
				writeAPIError(w, http.StatusUnprocessableEntity, "state_reason must be one of completed, not_planned, duplicate, reopened")
				return
			}
		}
		if err := issues.SetState(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, newState, reason); err != nil {
			writeIssuesError(w, err)
			return
		}
	}

	fresh, err := q.GetIssueByID(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusOK, presentIssue(fresh, h.labelNamesFor(r.Context(), fresh.ID)))
}

// ─── comments ───────────────────────────────────────────────────────

func (h *Handlers) issueCommentsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	issue, ok := h.resolveIssueByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := issuesdb.New().ListIssueComments(r.Context(), h.d.Pool, issue.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list comments", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]commentResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, presentComment(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type commentCreateRequest struct {
	Body string `json:"body"`
}

func (h *Handlers) issueCommentCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueComment)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, ok := h.resolveIssueByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body commentCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	isCollab := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(*repo)).Allow
	c, err := issues.AddComment(r.Context(), h.issuesDeps(), issues.CommentCreateParams{
		IssueID:      issue.ID,
		AuthorUserID: auth.UserID,
		Body:         body.Body,
		IsCollab:     isCollab,
	})
	if err != nil {
		writeIssuesError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentComment(c))
}

type commentUpdateRequest struct {
	Body string `json:"body"`
}

func (h *Handlers) issueCommentUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	q := issuesdb.New()
	comment, err := q.GetIssueComment(r.Context(), h.d.Pool, cid)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	// Cross-repo guard: the comment must belong to an issue in this
	// repo. Without this, a caller could /repos/foo/bar/issues/comments/{id}
	// against an unrelated comment id.
	issue, err := q.GetIssueByID(r.Context(), h.d.Pool, comment.IssueID)
	if err != nil || issue.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	if !canEditComment(comment, auth.UserID) {
		writeAPIError(w, http.StatusForbidden, "only the author may edit this comment")
		return
	}
	var body commentUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	trimmed := strings.TrimSpace(body.Body)
	if trimmed == "" {
		writeAPIError(w, http.StatusUnprocessableEntity, "body is required")
		return
	}
	if len(trimmed) > 65535 {
		writeAPIError(w, http.StatusUnprocessableEntity, "body too long")
		return
	}
	if err := q.UpdateIssueCommentBody(r.Context(), h.d.Pool, issuesdb.UpdateIssueCommentBodyParams{
		ID: comment.ID, Body: trimmed,
		// body_html_cached is cleared; the next render path picks the
		// fresh body up. Matches how the HTML comment editor handles
		// re-renders (lazy regeneration on read).
		BodyHtmlCached: pgtype.Text{},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: update comment", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "update failed")
		return
	}
	fresh, _ := q.GetIssueComment(r.Context(), h.d.Pool, comment.ID)
	writeJSON(w, http.StatusOK, presentComment(fresh))
}

func (h *Handlers) issueCommentDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	q := issuesdb.New()
	comment, err := q.GetIssueComment(r.Context(), h.d.Pool, cid)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	issue, err := q.GetIssueByID(r.Context(), h.d.Pool, comment.IssueID)
	if err != nil || issue.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "comment not found")
		return
	}
	// Delete is broader than edit: a repo collaborator with write
	// access can remove any comment (matches GitHub's "moderation"
	// affordance), the comment author can remove their own.
	canDelete := comment.AuthorUserID.Valid && comment.AuthorUserID.Int64 == auth.UserID
	if !canDelete {
		canDelete = policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(*repo)).Allow
	}
	if !canDelete {
		writeAPIError(w, http.StatusForbidden, "lack permission to delete this comment")
		return
	}
	if err := q.DeleteIssueComment(r.Context(), h.d.Pool, comment.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete comment", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func canEditComment(c issuesdb.IssueComment, actorUserID int64) bool {
	if !c.AuthorUserID.Valid {
		return false
	}
	return c.AuthorUserID.Int64 == actorUserID
}

// ─── lock ───────────────────────────────────────────────────────────

type issueLockRequest struct {
	Reason string `json:"lock_reason"`
}

func (h *Handlers) issueLock(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueClose)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, ok := h.resolveIssueByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body issueLockRequest
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional
	if err := issues.SetLock(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, true, body.Reason); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: lock", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lock failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) issueUnlock(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueClose)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, ok := h.resolveIssueByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	if err := issues.SetLock(r.Context(), h.issuesDeps(), auth.UserID, issue.ID, false, ""); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: unlock", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "unlock failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── helpers ────────────────────────────────────────────────────────

func (h *Handlers) resolveIssueByNumber(w http.ResponseWriter, r *http.Request, repoID int64, numberRaw string) (issuesdb.Issue, bool) {
	num, err := strconv.ParseInt(numberRaw, 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return issuesdb.Issue{}, false
	}
	issue, err := issuesdb.New().GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: repoID, Number: num,
	})
	if err != nil || issue.Kind != issuesdb.IssueKindIssue {
		writeAPIError(w, http.StatusNotFound, "issue not found")
		return issuesdb.Issue{}, false
	}
	return issue, true
}

func (h *Handlers) issuesDeps() issues.Deps {
	return issues.Deps{
		Pool:    h.d.Pool,
		Limiter: h.d.Throttle,
		Logger:  h.d.Logger,
		Audit:   h.d.Audit,
	}
}

func writeIssuesError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, issues.ErrEmptyTitle),
		errors.Is(err, issues.ErrTitleTooLong),
		errors.Is(err, issues.ErrBodyTooLong),
		errors.Is(err, issues.ErrEmptyComment),
		errors.Is(err, issues.ErrCommentTooLong):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, issues.ErrCommentRateLimit):
		writeAPIError(w, http.StatusTooManyRequests, "comment rate limit exceeded")
	case errors.Is(err, issues.ErrIssueLocked):
		writeAPIError(w, http.StatusLocked, "issue is locked")
	case errors.Is(err, issues.ErrIssueNotFound):
		writeAPIError(w, http.StatusNotFound, "issue not found")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
