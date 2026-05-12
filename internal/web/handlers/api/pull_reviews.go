// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/pulls/review"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountPullReviews registers the §4b PR review surface.
//
//	GET    /api/v1/repos/{o}/{r}/pulls/{number}/reviews                 list reviews
//	POST   /api/v1/repos/{o}/{r}/pulls/{number}/reviews                 submit review
//	GET    /api/v1/repos/{o}/{r}/pulls/{number}/comments                list inline comments
//	POST   /api/v1/repos/{o}/{r}/pulls/{number}/comments                add inline comment
//	GET    /api/v1/repos/{o}/{r}/pulls/{number}/requested_reviewers     list pending
//	POST   /api/v1/repos/{o}/{r}/pulls/{number}/requested_reviewers     request reviewer
//	DELETE /api/v1/repos/{o}/{r}/pulls/{number}/requested_reviewers     dismiss request
func (h *Handlers) mountPullReviews(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/pulls/{number}/reviews", h.pullReviewsList)
		r.Get("/api/v1/repos/{owner}/{repo}/pulls/{number}/comments", h.pullReviewCommentsList)
		r.Get("/api/v1/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", h.pullRequestedReviewersList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/pulls/{number}/reviews", h.pullReviewSubmit)
		r.Post("/api/v1/repos/{owner}/{repo}/pulls/{number}/comments", h.pullReviewCommentCreate)
		r.Post("/api/v1/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", h.pullRequestedReviewersAdd)
		r.Delete("/api/v1/repos/{owner}/{repo}/pulls/{number}/requested_reviewers", h.pullRequestedReviewersRemove)
	})
}

// ─── presentation ───────────────────────────────────────────────────

type reviewResponse struct {
	ID          int64  `json:"id"`
	PullID      int64  `json:"pull_id"`
	AuthorID    int64  `json:"author_id,omitempty"`
	State       string `json:"state"`
	Body        string `json:"body,omitempty"`
	SubmittedAt string `json:"submitted_at,omitempty"`
	Dismissed   bool   `json:"dismissed"`
	DismissedAt string `json:"dismissed_at,omitempty"`
}

func presentReview(r pullsdb.PrReview) reviewResponse {
	out := reviewResponse{
		ID:        r.ID,
		PullID:    r.PrIssueID,
		State:     string(r.State),
		Body:      r.Body,
		Dismissed: r.DismissedAt.Valid,
	}
	if r.AuthorUserID.Valid {
		out.AuthorID = r.AuthorUserID.Int64
	}
	if r.SubmittedAt.Valid {
		out.SubmittedAt = r.SubmittedAt.Time.UTC().Format(time.RFC3339)
	}
	if r.DismissedAt.Valid {
		out.DismissedAt = r.DismissedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

type reviewCommentResponse struct {
	ID                int64  `json:"id"`
	PullID            int64  `json:"pull_id"`
	ReviewID          int64  `json:"review_id,omitempty"`
	AuthorID          int64  `json:"author_id,omitempty"`
	FilePath          string `json:"file_path"`
	Side              string `json:"side"`
	OriginalCommitSHA string `json:"original_commit_sha"`
	OriginalLine      int32  `json:"original_line"`
	OriginalPosition  int32  `json:"original_position"`
	CurrentPosition   *int32 `json:"current_position,omitempty"`
	Body              string `json:"body"`
	InReplyToID       int64  `json:"in_reply_to_id,omitempty"`
	Pending           bool   `json:"pending"`
	Resolved          bool   `json:"resolved"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

func presentReviewComment(c pullsdb.PrReviewComment) reviewCommentResponse {
	out := reviewCommentResponse{
		ID:                c.ID,
		PullID:            c.PrIssueID,
		FilePath:          c.FilePath,
		Side:              string(c.Side),
		OriginalCommitSHA: c.OriginalCommitSha,
		OriginalLine:      c.OriginalLine,
		OriginalPosition:  c.OriginalPosition,
		Body:              c.Body,
		Pending:           c.Pending,
		Resolved:          c.ResolvedAt.Valid,
		CreatedAt:         c.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:         c.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if c.ReviewID.Valid {
		out.ReviewID = c.ReviewID.Int64
	}
	if c.AuthorUserID.Valid {
		out.AuthorID = c.AuthorUserID.Int64
	}
	if c.CurrentPosition.Valid {
		v := c.CurrentPosition.Int32
		out.CurrentPosition = &v
	}
	if c.InReplyToID.Valid {
		out.InReplyToID = c.InReplyToID.Int64
	}
	return out
}

type requestedReviewerResponse struct {
	ID              int64  `json:"id"`
	PullID          int64  `json:"pull_id"`
	UserID          int64  `json:"user_id"`
	RequestedByID   int64  `json:"requested_by_id,omitempty"`
	RequestedAt     string `json:"requested_at"`
	DismissedAt     string `json:"dismissed_at,omitempty"`
	SatisfiedReview int64  `json:"satisfied_by_review_id,omitempty"`
}

func presentRequest(r pullsdb.PrReviewRequest) requestedReviewerResponse {
	out := requestedReviewerResponse{
		ID:          r.ID,
		PullID:      r.PrIssueID,
		RequestedAt: r.RequestedAt.Time.UTC().Format(time.RFC3339),
	}
	if r.RequestedUserID.Valid {
		out.UserID = r.RequestedUserID.Int64
	}
	if r.RequestedByUserID.Valid {
		out.RequestedByID = r.RequestedByUserID.Int64
	}
	if r.DismissedAt.Valid {
		out.DismissedAt = r.DismissedAt.Time.UTC().Format(time.RFC3339)
	}
	if r.SatisfiedByReviewID.Valid {
		out.SatisfiedReview = r.SatisfiedByReviewID.Int64
	}
	return out
}

// ─── list reviews ───────────────────────────────────────────────────

func (h *Handlers) pullReviewsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := pullsdb.New().ListPRReviews(r.Context(), h.d.Pool, pr.IssueID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list pr reviews", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]reviewResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentReview(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── submit review ──────────────────────────────────────────────────

type reviewSubmitRequest struct {
	Event string `json:"event"`
	Body  string `json:"body"`
}

// reviewEventToState maps gh's API vocabulary (APPROVE / REQUEST_CHANGES
// / COMMENT) to our internal lowercase tags. Accepts the lowercase form
// too for clients that prefer it.
func reviewEventToState(event string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(event)) {
	case "approve", "approved":
		return "approve", true
	case "request_changes", "changes_requested":
		return "request_changes", true
	case "comment", "commented", "":
		return "comment", true
	default:
		return "", false
	}
}

func (h *Handlers) pullReviewSubmit(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	issue, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body reviewSubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	state, ok := reviewEventToState(body.Event)
	if !ok {
		writeAPIError(w, http.StatusUnprocessableEntity, "event must be APPROVE, REQUEST_CHANGES, or COMMENT")
		return
	}
	prAuthorID := int64(0)
	if issue.AuthorUserID.Valid {
		prAuthorID = issue.AuthorUserID.Int64
	}
	row, err := review.Submit(r.Context(), review.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, review.SubmitParams{
		PRIssueID:      pr.IssueID,
		AuthorUserID:   auth.UserID,
		State:          state,
		Body:           body.Body,
		PRAuthorUserID: prAuthorID,
	})
	if err != nil {
		writeReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentReview(row))
}

// ─── inline comments ────────────────────────────────────────────────

func (h *Handlers) pullReviewCommentsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := pullsdb.New().ListPRReviewComments(r.Context(), h.d.Pool, pr.IssueID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list pr review comments", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]reviewCommentResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, presentReviewComment(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type reviewCommentCreateRequest struct {
	Body              string `json:"body"`
	FilePath          string `json:"file_path"`
	Side              string `json:"side"`
	OriginalCommitSHA string `json:"original_commit_sha"`
	OriginalLine      int32  `json:"original_line"`
	OriginalPosition  int32  `json:"original_position"`
	CurrentPosition   int32  `json:"current_position"`
	InReplyToID       int64  `json:"in_reply_to_id"`
	Pending           bool   `json:"pending"`
}

func (h *Handlers) pullReviewCommentCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body reviewCommentCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// current_position defaults to original_position when the caller
	// omits it (matches the HTML form's "cur ?= pos" fallback).
	cur := body.CurrentPosition
	if cur == 0 && body.InReplyToID == 0 {
		cur = body.OriginalPosition
	}
	c, err := review.AddComment(r.Context(), review.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, review.CommentParams{
		PRIssueID:         pr.IssueID,
		AuthorUserID:      auth.UserID,
		FilePath:          body.FilePath,
		Side:              body.Side,
		OriginalCommitSHA: body.OriginalCommitSHA,
		OriginalLine:      body.OriginalLine,
		OriginalPosition:  body.OriginalPosition,
		CurrentPosition:   cur,
		Body:              body.Body,
		InReplyToID:       body.InReplyToID,
		Pending:           body.Pending,
	})
	if err != nil {
		writeReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentReviewComment(c))
}

// ─── requested reviewers ────────────────────────────────────────────

func (h *Handlers) pullRequestedReviewersList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	rows, err := pullsdb.New().ListPRReviewRequests(r.Context(), h.d.Pool, pr.IssueID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list review requests", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]requestedReviewerResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentRequest(row))
	}
	writeJSON(w, http.StatusOK, out)
}

type requestedReviewerCreateRequest struct {
	// Either Username or UserID identifies the reviewer; if both are
	// present UserID wins. UserID is the stable handle; Username is
	// the gh-compatible form.
	Username string `json:"username"`
	UserID   int64  `json:"user_id"`
}

func (h *Handlers) pullRequestedReviewersAdd(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body requestedReviewerCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	reviewerID, err := h.resolveReviewerID(r, body)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	row, err := review.Request(r.Context(), review.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, review.RequestParams{
		PRIssueID:         pr.IssueID,
		RequestedUserID:   reviewerID,
		RequestedByUserID: auth.UserID,
	})
	if err != nil {
		writeReviewError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentRequest(row))
}

func (h *Handlers) pullRequestedReviewersRemove(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	_, pr, ok := h.resolvePRByNumber(w, r, repo.ID, chi.URLParam(r, "number"))
	if !ok {
		return
	}
	var body requestedReviewerCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	reviewerID, err := h.resolveReviewerID(r, body)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	q := pullsdb.New()
	rows, err := q.ListPRReviewRequests(r.Context(), h.d.Pool, pr.IssueID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list review requests", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	for _, row := range rows {
		if !row.RequestedUserID.Valid || row.RequestedUserID.Int64 != reviewerID {
			continue
		}
		if row.DismissedAt.Valid || row.SatisfiedByReviewID.Valid {
			continue
		}
		_ = auth // dismissal currently records no actor; matches the existing HTML surface
		if err := q.DismissPRReviewRequest(r.Context(), h.d.Pool, row.ID); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: dismiss review request", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "delete failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeAPIError(w, http.StatusNotFound, "no active review request for that user")
}

func (h *Handlers) resolveReviewerID(r *http.Request, body requestedReviewerCreateRequest) (int64, error) {
	if body.UserID != 0 {
		return body.UserID, nil
	}
	if strings.TrimSpace(body.Username) == "" {
		return 0, errors.New("username or user_id is required")
	}
	user, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, body.Username)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, errors.New("reviewer not found")
		}
		return 0, err
	}
	return user.ID, nil
}

// ─── helpers ────────────────────────────────────────────────────────

func writeReviewError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, review.ErrEmptyBody),
		errors.Is(err, review.ErrBodyTooLong),
		errors.Is(err, review.ErrInvalidState):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, review.ErrAuthorCannotApprove):
		writeAPIError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, review.ErrCommentNotOnPR),
		errors.Is(err, review.ErrReviewNotFound):
		writeAPIError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, review.ErrReviewerLimitReached):
		writeAPIError(w, http.StatusUnprocessableEntity, "20 reviewers max per PR")
	case errors.Is(err, review.ErrReviewerAlreadyPending):
		writeAPIError(w, http.StatusConflict, "reviewer already requested")
	case errors.Is(err, review.ErrAlreadyResolved),
		errors.Is(err, review.ErrNotResolved):
		writeAPIError(w, http.StatusConflict, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
