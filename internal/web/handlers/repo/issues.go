// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountIssues registers the issues + labels + milestones routes under
// /{owner}/{repo}. Read paths are public (subject to policy.Can which
// gates private repos); write paths run through RequireUser so an
// anonymous browser hits the login redirect rather than the 404
// existence-leak path.
func (h *Handlers) MountIssues(r chi.Router) {
	// Public reads.
	r.Get("/{owner}/{repo}/issues", h.issuesList)
	r.Get("/{owner}/{repo}/issues/{number}", h.issueView)
	r.Get("/{owner}/{repo}/labels", h.labelsList)
	r.Get("/{owner}/{repo}/milestones", h.milestonesList)

	// Auth-required: form GETs + state-changing POSTs. policy.Can
	// inside the handler still applies (e.g. archived repos block
	// writes even for the owner), but RequireUser handles the simpler
	// "you need to be logged in" case with a /login redirect.
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		r.Get("/{owner}/{repo}/issues/new", h.issueNewForm)
		r.Post("/{owner}/{repo}/issues", h.issueCreate)
		r.Post("/{owner}/{repo}/issues/{number}/comments", h.issueComment)
		r.Post("/{owner}/{repo}/issues/{number}/state", h.issueSetState)
		r.Post("/{owner}/{repo}/issues/{number}/lock", h.issueSetLock)
		r.Post("/{owner}/{repo}/issues/{number}/labels", h.issueApplyLabels)
		r.Post("/{owner}/{repo}/issues/{number}/milestone", h.issueAssignMilestone)
		r.Post("/{owner}/{repo}/issues/{number}/assignees", h.issueToggleAssignee)

		r.Post("/{owner}/{repo}/labels", h.labelCreate)
		r.Post("/{owner}/{repo}/labels/{id}/update", h.labelUpdate)
		r.Post("/{owner}/{repo}/labels/{id}/delete", h.labelDelete)

		r.Post("/{owner}/{repo}/milestones", h.milestoneCreate)
		r.Post("/{owner}/{repo}/milestones/{id}/update", h.milestoneUpdate)
		r.Post("/{owner}/{repo}/milestones/{id}/state", h.milestoneSetState)
		r.Post("/{owner}/{repo}/milestones/{id}/delete", h.milestoneDelete)
	})
}

// issuesDeps materializes an issues.Deps from the handler-set deps.
// Limiter is the same per-process instance used by repo create — the
// orchestrator scopes by Identifier so namespaces don't collide.
func (h *Handlers) issuesDeps() issues.Deps {
	return issues.Deps{
		Pool:    h.d.Pool,
		Limiter: h.d.Limiter,
		Logger:  h.d.Logger,
		Audit:   h.d.Audit,
	}
}

// issuesList renders /{owner}/{repo}/issues with optional ?state filter.
func (h *Handlers) issuesList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	stateFilter := r.URL.Query().Get("state")
	if stateFilter == "" {
		stateFilter = "open"
	}
	stateNarg := pgtype.Text{}
	if stateFilter == "open" || stateFilter == "closed" {
		stateNarg = pgtype.Text{String: stateFilter, Valid: true}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const perPage = 25
	rows, err := h.iq.ListIssues(r.Context(), h.d.Pool, issuesdb.ListIssuesParams{
		RepoID:      row.ID,
		StateFilter: stateNarg,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
		Limit:       perPage,
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "issues: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	total, _ := h.iq.CountIssues(r.Context(), h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      row.ID,
		StateFilter: stateNarg,
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})
	openCount, _ := h.iq.CountIssues(r.Context(), h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      row.ID,
		StateFilter: pgtype.Text{String: "open", Valid: true},
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})
	closedCount, _ := h.iq.CountIssues(r.Context(), h.d.Pool, issuesdb.CountIssuesParams{
		RepoID:      row.ID,
		StateFilter: pgtype.Text{String: "closed", Valid: true},
		Kind:        issuesdb.NullIssueKind{IssueKind: issuesdb.IssueKindIssue, Valid: true},
	})

	// Decorate with author username + label set + assignee count for the
	// list. Cheap N+1 for v1; S36 will batch this.
	type listItem struct {
		Issue        issuesdb.Issue
		AuthorName   string
		Labels       []issuesdb.Label
		Assignees    []issuesdb.ListIssueAssigneesRow
		CommentCount int64
	}
	items := make([]listItem, 0, len(rows))
	for _, ir := range rows {
		it := listItem{Issue: ir}
		if ir.AuthorUserID.Valid {
			if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, ir.AuthorUserID.Int64); err == nil {
				it.AuthorName = u.Username
			}
		}
		it.Labels, _ = h.iq.ListLabelsOnIssue(r.Context(), h.d.Pool, ir.ID)
		it.Assignees, _ = h.iq.ListIssueAssignees(r.Context(), h.d.Pool, ir.ID)
		items = append(items, it)
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, "repo/issues_list", map[string]any{
		"Title":       "Issues · " + row.Name,
		"Owner":       owner.Username,
		"Repo":        row,
		"Items":       items,
		"State":       stateFilter,
		"OpenCount":   openCount,
		"ClosedCount": closedCount,
		"Total":       total,
		"Page":        page,
		"PerPage":     perPage,
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "issues: render list", "error", err)
	}
}

// issueNewForm renders the new-issue form.
func (h *Handlers) issueNewForm(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueCreate)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/issue_new", map[string]any{
		"Title":     "New issue · " + row.Name,
		"Owner":     owner.Username,
		"Repo":      row,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
	})
}

// issueCreate handles the new-issue POST.
func (h *Handlers) issueCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueCreate)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	title := strings.TrimSpace(r.PostFormValue("title"))
	body := r.PostFormValue("body")
	created, err := issues.Create(r.Context(), h.issuesDeps(), issues.CreateParams{
		RepoID:       row.ID,
		AuthorUserID: viewer.ID,
		Title:        title,
		Body:         body,
		Kind:         "issue",
	})
	if err != nil {
		h.renderIssueCreateError(w, r, owner.Username, row, title, body, err)
		return
	}
	// Auto-watch on first involvement (S26): subscribe the author at
	// `participating` so notifications fan-out (S29) routes future
	// thread events to them. Non-destructive — no-op if the user
	// already has an explicit preference.
	_ = social.AutoWatchOnInvolvement(r.Context(), h.socialDeps(), viewer.ID, row.ID)
	http.Redirect(w, r,
		"/"+owner.Username+"/"+row.Name+"/issues/"+strconv.FormatInt(created.Number, 10),
		http.StatusSeeOther,
	)
}

func (h *Handlers) renderIssueCreateError(w http.ResponseWriter, r *http.Request, owner string, row reposdb.Repo, title, body string, err error) {
	msg := "Could not create the issue. Try again."
	switch {
	case errors.Is(err, issues.ErrEmptyTitle):
		msg = "Title is required."
	case errors.Is(err, issues.ErrTitleTooLong):
		msg = "Title is too long (max 256)."
	case errors.Is(err, issues.ErrBodyTooLong):
		msg = "Body is too long."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = h.d.Render.RenderPage(w, r, "repo/issue_new", map[string]any{
		"Title":     "New issue · " + row.Name,
		"Owner":     owner,
		"Repo":      row,
		"FormTitle": title,
		"FormBody":  body,
		"Error":     msg,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
	})
}

// issueView renders /{owner}/{repo}/issues/{number} with the timeline.
func (h *Handlers) issueView(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	num, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	issue, err := h.iq.GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: row.ID, Number: num,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "issues: get", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	comments, _ := h.iq.ListIssueComments(r.Context(), h.d.Pool, issue.ID)
	events, _ := h.iq.ListIssueEvents(r.Context(), h.d.Pool, issue.ID)
	labels, _ := h.iq.ListLabelsOnIssue(r.Context(), h.d.Pool, issue.ID)
	assignees, _ := h.iq.ListIssueAssignees(r.Context(), h.d.Pool, issue.ID)
	allLabels, _ := h.iq.ListLabels(r.Context(), h.d.Pool, row.ID)
	milestones, _ := h.iq.ListMilestones(r.Context(), h.d.Pool, row.ID)

	// Resolve usernames for comment authors.
	type commentRow struct {
		C          issuesdb.IssueComment
		AuthorName string
	}
	cs := make([]commentRow, 0, len(comments))
	for _, c := range comments {
		cr := commentRow{C: c}
		if c.AuthorUserID.Valid {
			if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, c.AuthorUserID.Int64); err == nil {
				cr.AuthorName = u.Username
			}
		}
		cs = append(cs, cr)
	}
	// Author username on the issue itself.
	authorName := ""
	if issue.AuthorUserID.Valid {
		if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, issue.AuthorUserID.Int64); err == nil {
			authorName = u.Username
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/issue_view", map[string]any{
		"Title":      issue.Title + " · " + row.Name,
		"Owner":      owner.Username,
		"Repo":       row,
		"Issue":      issue,
		"AuthorName": authorName,
		"Comments":   cs,
		"Events":     events,
		"Labels":     labels,
		"Assignees":  assignees,
		"AllLabels":  allLabels,
		"Milestones": milestones,
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
	})
}

func (h *Handlers) loadIssueByNumber(w http.ResponseWriter, r *http.Request, repo reposdb.Repo) (issuesdb.Issue, bool) {
	num, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return issuesdb.Issue{}, false
	}
	issue, err := h.iq.GetIssueByNumber(r.Context(), h.d.Pool, issuesdb.GetIssueByNumberParams{
		RepoID: repo.ID, Number: num,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return issuesdb.Issue{}, false
	}
	return issue, true
}

// issueComment handles POST .../comments
func (h *Handlers) issueComment(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueComment)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	body := r.PostFormValue("body")

	// IsCollab is the locked-issue bypass: triage+ on the repo can comment
	// past a `locked=true` flag (the gate exists to silence drive-by
	// posters). We resolve the *real* role via the policy package — owner
	// is implicit admin, and any explicit collaborator with role >= triage
	// passes. Read fails (DB miss, unknown role) fail closed via RoleNone.
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	isCollab := policy.HasRoleAtLeast(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.NewRepoRefFromRepo(row), policy.RoleTriage)

	_, err := issues.AddComment(r.Context(), h.issuesDeps(), issues.CommentCreateParams{
		IssueID:      issue.ID,
		AuthorUserID: viewer.ID,
		Body:         body,
		IsCollab:     isCollab,
	})
	if err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
		return
	}
	// Auto-watch on first involvement (S26).
	_ = social.AutoWatchOnInvolvement(r.Context(), h.socialDeps(), viewer.ID, row.ID)
	h.redirectIssue(w, r, owner.Username, row.Name, issue.Number)
}

func (h *Handlers) issueSetState(w http.ResponseWriter, r *http.Request) {
	// Two-pass authorization: read access first, then ActionIssueClose
	// with `repo.AuthorUserID = issue.AuthorUserID` set so the policy
	// engine grants author-self-close. Without the second pass, an
	// issue's reporter who isn't a triage collaborator couldn't close
	// their own thread — which the audit flagged (S00-S25, H1).
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	repoRef := policy.NewRepoRefFromRepo(row)
	if issue.AuthorUserID.Valid {
		repoRef.AuthorUserID = issue.AuthorUserID.Int64
	}
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionIssueClose, repoRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return
	}
	state := strings.TrimSpace(r.PostFormValue("state"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if err := issues.SetState(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, state, reason); err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
		return
	}
	h.redirectIssue(w, r, owner.Username, row.Name, issue.Number)
}

func (h *Handlers) issueSetLock(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueClose)
	if !ok {
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	locked := r.PostFormValue("lock") == "true"
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := issues.SetLock(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, locked, reason); err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
		return
	}
	h.redirectIssue(w, r, owner.Username, row.Name, issue.Number)
}

func (h *Handlers) issueApplyLabels(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	raw := r.PostForm["label_ids"]
	ids := make([]int64, 0, len(raw))
	for _, s := range raw {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil {
			ids = append(ids, id)
		}
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := issues.ApplyLabels(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, ids); err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
		return
	}
	h.redirectIssue(w, r, owner.Username, row.Name, issue.Number)
}

func (h *Handlers) issueAssignMilestone(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	mid, _ := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("milestone_id")), 10, 64)
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := issues.AssignMilestone(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, mid); err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
		return
	}
	h.redirectIssue(w, r, owner.Username, row.Name, issue.Number)
}

func (h *Handlers) issueToggleAssignee(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionIssueAssign)
	if !ok {
		return
	}
	issue, ok := h.loadIssueByNumber(w, r, row)
	if !ok {
		return
	}
	target := strings.TrimSpace(r.PostFormValue("username"))
	mode := r.PostFormValue("mode") // "add" | "remove"
	tu, err := h.uq.GetUserByUsername(r.Context(), h.d.Pool, target)
	if err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, errors.New("user not found"))
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if mode == "remove" {
		err = issues.UnassignUser(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, tu.ID)
	} else {
		err = issues.AssignUser(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, tu.ID)
	}
	if err != nil {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
		return
	}
	h.redirectIssue(w, r, owner.Username, row.Name, issue.Number)
}

func (h *Handlers) redirectIssue(w http.ResponseWriter, r *http.Request, owner, repo string, number int64) {
	http.Redirect(w, r, "/"+owner+"/"+repo+"/issues/"+strconv.FormatInt(number, 10), http.StatusSeeOther)
}

func (h *Handlers) handleIssueWriteError(w http.ResponseWriter, r *http.Request, _ string, _ reposdb.Repo, _ issuesdb.Issue, err error) {
	switch {
	case errors.Is(err, issues.ErrIssueLocked):
		h.d.Render.HTTPError(w, r, http.StatusLocked, "issue is locked")
	case errors.Is(err, issues.ErrCommentRateLimit):
		h.d.Render.HTTPError(w, r, http.StatusTooManyRequests, "rate limit")
	case errors.Is(err, issues.ErrCommentTooLong):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "comment too long")
	default:
		var t *throttle.ErrThrottled
		if errors.As(err, &t) {
			h.d.Render.HTTPError(w, r, http.StatusTooManyRequests, "rate limit")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "issues: write", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}
