// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	"github.com/tenseleyFlow/shithub/internal/pulls/review"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// MountPullReview registers the S23 review-related routes under the
// existing pulls router. All routes require auth (writes only).
func (h *Handlers) MountPullReview(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		// Inline review comments + threads.
		r.Post("/{owner}/{repo}/pulls/{number}/review-comments", h.prCommentCreate)
		r.Post("/{owner}/{repo}/pulls/{number}/review-comments/{cid}/reply", h.prCommentReply)
		r.Post("/{owner}/{repo}/pulls/{number}/review-comments/{cid}/edit", h.prCommentEdit)
		r.Post("/{owner}/{repo}/pulls/{number}/review-comments/{cid}/resolve", h.prCommentResolve)
		r.Post("/{owner}/{repo}/pulls/{number}/review-comments/{cid}/reopen", h.prCommentReopen)

		// Reviews.
		r.Post("/{owner}/{repo}/pulls/{number}/reviews", h.prReviewSubmit)
		r.Post("/{owner}/{repo}/pulls/{number}/reviews/{rid}/dismiss", h.prReviewDismiss)

		// Reviewer requests.
		r.Post("/{owner}/{repo}/pulls/{number}/reviewers", h.prReviewerRequest)
	})
}

// reviewDeps materializes the review.Deps from the handler set.
func (h *Handlers) reviewDeps() review.Deps {
	return review.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
}

// kickMergeability enqueues a pr:mergeability job after review-side
// writes and when an open PR view detects a still-unknown merge state.
func (h *Handlers) kickMergeability(r *http.Request, prID int64) {
	if _, err := worker.Enqueue(r.Context(), h.d.Pool, worker.KindPRMergeability,
		map[string]any{"pr_id": prID}, worker.EnqueueOptions{}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "pulls: enqueue mergeability", "error", err)
	}
	_ = worker.Notify(r.Context(), h.d.Pool)
}

func (h *Handlers) prCommentCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	line, _ := strconv.Atoi(r.PostFormValue("line"))
	pos, _ := strconv.Atoi(r.PostFormValue("position"))
	cur, _ := strconv.Atoi(r.PostFormValue("current_position"))
	if cur == 0 {
		cur = pos
	}
	pending := r.PostFormValue("pending") == "on"
	if _, err := review.AddComment(r.Context(), h.reviewDeps(), review.CommentParams{
		PRIssueID:         pr.IID,
		AuthorUserID:      viewer.ID,
		FilePath:          r.PostFormValue("file_path"),
		Side:              r.PostFormValue("side"),
		OriginalCommitSHA: r.PostFormValue("commit_sha"),
		OriginalLine:      int32(line),
		OriginalPosition:  int32(pos),
		CurrentPosition:   int32(cur),
		Body:              r.PostFormValue("body"),
		Pending:           pending,
	}); err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	h.redirectPullTab(w, r, owner.Username, row.Name, pr.INumber, "files")
}

func (h *Handlers) prCommentReply(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := review.AddComment(r.Context(), h.reviewDeps(), review.CommentParams{
		PRIssueID:    pr.IID,
		AuthorUserID: viewer.ID,
		Body:         r.PostFormValue("body"),
		InReplyToID:  cid,
	}); err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	h.redirectPullTab(w, r, owner.Username, row.Name, pr.INumber, "files")
}

func (h *Handlers) prCommentEdit(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	c, err := pullsdb.New().GetPRReviewComment(r.Context(), h.d.Pool, cid)
	if err != nil || c.PrIssueID != pr.IID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !c.AuthorUserID.Valid || c.AuthorUserID.Int64 != viewer.ID {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "only the author can edit")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	if err := review.EditComment(r.Context(), h.reviewDeps(), cid, r.PostFormValue("body")); err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	h.redirectPullTab(w, r, owner.Username, row.Name, pr.INumber, "files")
}

func (h *Handlers) prCommentResolve(w http.ResponseWriter, r *http.Request) {
	h.commentResolveOrReopen(w, r, true)
}

func (h *Handlers) prCommentReopen(w http.ResponseWriter, r *http.Request) {
	h.commentResolveOrReopen(w, r, false)
}

func (h *Handlers) commentResolveOrReopen(w http.ResponseWriter, r *http.Request, resolve bool) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	cid, err := strconv.ParseInt(chi.URLParam(r, "cid"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if resolve {
		err = review.Resolve(r.Context(), h.reviewDeps(), viewer.ID, cid)
	} else {
		err = review.Reopen(r.Context(), h.reviewDeps(), cid)
	}
	if err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	h.redirectPullTab(w, r, owner.Username, row.Name, pr.INumber, "files")
}

func (h *Handlers) prReviewSubmit(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	prAuthor := int64(0)
	if pr.IAuthorUserID.Valid {
		prAuthor = pr.IAuthorUserID.Int64
	}
	if _, err := review.Submit(r.Context(), h.reviewDeps(), review.SubmitParams{
		PRIssueID:      pr.IID,
		AuthorUserID:   viewer.ID,
		State:          strings.TrimSpace(r.PostFormValue("state")),
		Body:           r.PostFormValue("body"),
		PRAuthorUserID: prAuthor,
	}); err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	// Refresh mergeability so the merge button reflects the new state.
	h.kickMergeability(r, pr.IID)
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

func (h *Handlers) prReviewDismiss(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullMerge)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	rid, err := strconv.ParseInt(chi.URLParam(r, "rid"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := review.Dismiss(r.Context(), h.reviewDeps(), viewer.ID, rid, r.PostFormValue("reason")); err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	h.kickMergeability(r, pr.IID)
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

func (h *Handlers) prReviewerRequest(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullReview)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	target, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, username)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "user not found")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if _, err := review.Request(r.Context(), h.reviewDeps(), review.RequestParams{
		PRIssueID:         pr.IID,
		RequestedUserID:   target.ID,
		RequestedByUserID: viewer.ID,
	}); err != nil {
		h.handleReviewWriteError(w, r, err)
		return
	}
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

func (h *Handlers) handleReviewWriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, review.ErrEmptyBody):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "comment body required")
	case errors.Is(err, review.ErrBodyTooLong):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "comment too long")
	case errors.Is(err, review.ErrAuthorCannotApprove):
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "you can't approve your own PR")
	case errors.Is(err, review.ErrInvalidState):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "review state must be comment, approve, or request_changes")
	case errors.Is(err, review.ErrCommentNotOnPR):
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "comment not found")
	case errors.Is(err, review.ErrAlreadyResolved):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "thread already resolved")
	case errors.Is(err, review.ErrNotResolved):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "thread is not resolved")
	case errors.Is(err, review.ErrReviewerLimitReached):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "reviewer limit (20) reached")
	case errors.Is(err, review.ErrReviewerAlreadyPending):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "reviewer already requested")
	case errors.Is(err, pulls.ErrPRNotFound):
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
	default:
		h.d.Logger.WarnContext(r.Context(), "review: write", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}

func (h *Handlers) redirectPullTab(w http.ResponseWriter, r *http.Request, owner, repo string, number int64, tab string) {
	url := "/" + owner + "/" + repo + "/pulls/" + strconv.FormatInt(number, 10)
	if tab != "" && tab != "conversation" {
		url += "/" + tab
	}
	http.Redirect(w, r, url, http.StatusSeeOther)
}
