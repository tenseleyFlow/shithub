// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// MountPulls registers /{owner}/{repo}/pulls* routes. Reads are
// public (subject to policy.Can(ActionPullRead)); writes require auth.
// The merge route runs synchronously inside the request: pulls.Merge
// performs the worktree operation, updates DB state, and the response
// redirects the user straight to the merged view. (An async-merge path
// can be reintroduced when very-large-repo merges become a real
// concern; the worker registration was deleted alongside the unused
// KindPRMerge in the audit remediation sprint.)
func (h *Handlers) MountPulls(r chi.Router) {
	r.Get("/{owner}/{repo}/pulls", h.pullsList)
	r.Get("/{owner}/{repo}/pulls/{number}", h.pullView)
	r.Get("/{owner}/{repo}/pulls/{number}/files", h.pullFiles)
	r.Get("/{owner}/{repo}/pulls/{number}/commits", h.pullCommits)
	r.Get("/{owner}/{repo}/pulls/{number}/checks", h.pullChecks)

	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireUser)
		r.Get("/{owner}/{repo}/pulls/new", h.pullNewForm)
		r.Post("/{owner}/{repo}/pulls", h.pullCreate)
		r.Post("/{owner}/{repo}/pulls/{number}/edit", h.pullEdit)
		r.Post("/{owner}/{repo}/pulls/{number}/state", h.pullSetState)
		r.Post("/{owner}/{repo}/pulls/{number}/ready", h.pullSetReady)
		r.Post("/{owner}/{repo}/pulls/{number}/merge", h.pullMerge)
	})
	// S23 review surface — its own group so the auth-required wrapper
	// is shared cleanly without rewriting this file's existing one.
	h.MountPullReview(r)
}

func (h *Handlers) pullsDeps() pulls.Deps {
	return pulls.Deps{Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit}
}

// pullsList renders /{owner}/{repo}/pulls.
func (h *Handlers) pullsList(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	stateFilter := pgtype.Text{}
	if state == "open" || state == "closed" {
		stateFilter = pgtype.Text{String: state, Valid: true}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const perPage = 25
	q := pullsdb.New()
	rows, err := q.ListPullRequestsByRepo(r.Context(), h.d.Pool, pullsdb.ListPullRequestsByRepoParams{
		RepoID:      row.ID,
		StateFilter: stateFilter,
		Limit:       perPage,
		Offset:      int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "pulls: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	openCount, _ := q.CountPullRequestsByRepo(r.Context(), h.d.Pool, pullsdb.CountPullRequestsByRepoParams{
		RepoID: row.ID, StateFilter: pgtype.Text{String: "open", Valid: true},
	})
	closedCount, _ := q.CountPullRequestsByRepo(r.Context(), h.d.Pool, pullsdb.CountPullRequestsByRepoParams{
		RepoID: row.ID, StateFilter: pgtype.Text{String: "closed", Valid: true},
	})

	type listItem struct {
		Row        pullsdb.ListPullRequestsByRepoRow
		AuthorName string
	}
	items := make([]listItem, 0, len(rows))
	for _, lr := range rows {
		it := listItem{Row: lr}
		if lr.AuthorUserID.Valid {
			if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, lr.AuthorUserID.Int64); err == nil {
				it.AuthorName = u.Username
			}
		}
		items = append(items, it)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/pulls_list", map[string]any{
		"Title":       "Pull requests · " + row.Name,
		"Owner":       owner.Username,
		"Repo":        row,
		"Items":       items,
		"State":       state,
		"OpenCount":   openCount,
		"ClosedCount": closedCount,
		"Page":        page,
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
	})
}

// pullNewForm renders the open-PR form. base and head come from the
// query string (typically the compare view's "Open PR" link).
func (h *Handlers) pullNewForm(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullCreate)
	if !ok {
		return
	}
	base := r.URL.Query().Get("base")
	if base == "" {
		base = row.DefaultBranch
	}
	head := r.URL.Query().Get("head")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/pull_new", map[string]any{
		"Title":     "New pull request · " + row.Name,
		"Owner":     owner.Username,
		"Repo":      row,
		"Base":      base,
		"Head":      head,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
	})
}

// pullCreate handles POST /{owner}/{repo}/pulls.
func (h *Handlers) pullCreate(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullCreate)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	res, err := pulls.Create(r.Context(), h.pullsDeps(), pulls.CreateParams{
		RepoID:       row.ID,
		AuthorUserID: viewer.ID,
		Title:        r.PostFormValue("title"),
		Body:         r.PostFormValue("body"),
		BaseRef:      r.PostFormValue("base"),
		HeadRef:      r.PostFormValue("head"),
		Draft:        r.PostFormValue("draft") == "on",
		GitDir:       gitDir,
	})
	if err != nil {
		h.handlePullCreateError(w, r, owner.Username, row, err)
		return
	}
	// Kick off the mergeability probe right away.
	if _, err := worker.Enqueue(r.Context(), h.d.Pool, worker.KindPRMergeability,
		map[string]any{"pr_id": res.PullRequest.IssueID}, worker.EnqueueOptions{}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "pulls: enqueue mergeability", "error", err)
	}
	_ = worker.Notify(r.Context(), h.d.Pool)
	http.Redirect(w, r,
		"/"+owner.Username+"/"+row.Name+"/pulls/"+strconv.FormatInt(res.Issue.Number, 10),
		http.StatusSeeOther,
	)
}

func (h *Handlers) handlePullCreateError(w http.ResponseWriter, r *http.Request, owner string, row reposdb.Repo, err error) {
	msg := "Could not open the pull request."
	switch {
	case errors.Is(err, pulls.ErrSameBranch):
		msg = "Base and head must differ."
	case errors.Is(err, pulls.ErrBaseNotFound):
		msg = "Base branch not found."
	case errors.Is(err, pulls.ErrHeadNotFound):
		msg = "Head branch not found."
	case errors.Is(err, pulls.ErrNoCommitsToMerge):
		msg = "Head has no commits ahead of base."
	case errors.Is(err, issues.ErrEmptyTitle):
		msg = "Title is required."
	case errors.Is(err, issues.ErrTitleTooLong):
		msg = "Title is too long (max 256)."
	case errors.Is(err, issues.ErrBodyTooLong):
		msg = "Body is too long."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = h.d.Render.RenderPage(w, r, "repo/pull_new", map[string]any{
		"Title":     "New pull request · " + row.Name,
		"Owner":     owner,
		"Repo":      row,
		"Base":      r.PostFormValue("base"),
		"Head":      r.PostFormValue("head"),
		"FormTitle": r.PostFormValue("title"),
		"FormBody":  r.PostFormValue("body"),
		"Error":     msg,
		"CSRFToken": middleware.CSRFTokenForRequest(r),
	})
}

// loadPullByNumber resolves the URL number into the joined PR + issue
// row; renders 404 on miss.
func (h *Handlers) loadPullByNumber(w http.ResponseWriter, r *http.Request, repoID int64) (pullsdb.GetPullRequestByRepoAndNumberRow, bool) {
	num, err := strconv.ParseInt(chi.URLParam(r, "number"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return pullsdb.GetPullRequestByRepoAndNumberRow{}, false
	}
	row, err := pullsdb.New().GetPullRequestByRepoAndNumber(r.Context(), h.d.Pool, pullsdb.GetPullRequestByRepoAndNumberParams{
		RepoID: repoID, Number: num,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "pulls: load", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return pullsdb.GetPullRequestByRepoAndNumberRow{}, false
	}
	return row, true
}

// renderPullPage is the common preamble for the four PR tab views.
func (h *Handlers) renderPullPage(w http.ResponseWriter, r *http.Request, tab string, extras map[string]any) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	authorName := ""
	if pr.IAuthorUserID.Valid {
		if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, pr.IAuthorUserID.Int64); err == nil {
			authorName = u.Username
		}
	}
	data := map[string]any{
		"Title":      "#" + strconv.FormatInt(pr.INumber, 10) + " " + pr.ITitle + " · " + row.Name,
		"Owner":      owner.Username,
		"Repo":       row,
		"PR":         pr,
		"AuthorName": authorName,
		"Tab":        tab,
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
	}
	for k, v := range extras {
		data[k] = v
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/pull_view", data)
}

// pullView renders the Conversation tab.
func (h *Handlers) pullView(w http.ResponseWriter, r *http.Request) {
	// Resolve to grab issue id for comments+events.
	row, _, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	comments, _ := h.iq.ListIssueComments(r.Context(), h.d.Pool, pr.IID)
	events, _ := h.iq.ListIssueEvents(r.Context(), h.d.Pool, pr.IID)

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
	// Reviews + reviewer requests for the Conversation sidebar.
	reviews, _ := h.pq.ListPRReviews(r.Context(), h.d.Pool, pr.IID)
	type reviewRow struct {
		R          pullsdb.PrReview
		AuthorName string
	}
	rs := make([]reviewRow, 0, len(reviews))
	for _, rv := range reviews {
		rr := reviewRow{R: rv}
		if rv.AuthorUserID.Valid {
			if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, rv.AuthorUserID.Int64); err == nil {
				rr.AuthorName = u.Username
			}
		}
		rs = append(rs, rr)
	}
	requests, _ := h.pq.ListPRReviewRequests(r.Context(), h.d.Pool, pr.IID)
	type reqRow struct {
		R        pullsdb.PrReviewRequest
		Username string
	}
	reqs := make([]reqRow, 0, len(requests))
	for _, rq := range requests {
		rr := reqRow{R: rq}
		if rq.RequestedUserID.Valid {
			if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, rq.RequestedUserID.Int64); err == nil {
				rr.Username = u.Username
			}
		}
		reqs = append(reqs, rr)
	}
	h.renderPullPage(w, r, "conversation", map[string]any{
		"Comments":       cs,
		"Events":         events,
		"Reviews":        rs,
		"ReviewRequests": reqs,
	})
}

// pullCommits renders the Commits tab.
func (h *Handlers) pullCommits(w http.ResponseWriter, r *http.Request) {
	row, _, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	commits, _ := pullsdb.New().ListPullRequestCommits(r.Context(), h.d.Pool, pr.IID)
	h.renderPullPage(w, r, "commits", map[string]any{
		"Commits": commits,
	})
}

// pullFiles renders the Files Changed tab. Uses the existing diff
// renderer fed from base..head (three-dot via FromMergeBase).
func (h *Handlers) pullFiles(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	files, _ := pullsdb.New().ListPullRequestFiles(r.Context(), h.d.Pool, pr.IID)
	diffHTML := ""
	if pr.BaseOid != "" && pr.HeadOid != "" {
		patch, perr := compareSourceMergeBase(r, gitDir, pr.BaseOid, pr.HeadOid)
		if perr == nil {
			diffHTML = renderCompareDiff(patch)
		}
	}
	// Per-file inline review threads. v1 groups by file_path; the
	// Files tab shows them collapsed under each section. Position-
	// mapped comments display inline; outdated ones are hidden by
	// default behind the "Show outdated" toggle.
	type commentRow struct {
		C          pullsdb.PrReviewComment
		AuthorName string
	}
	threadsByFile := map[string][]commentRow{}
	for _, f := range files {
		rows, _ := h.pq.ListPRReviewCommentsForFile(r.Context(), h.d.Pool, pullsdb.ListPRReviewCommentsForFileParams{
			PrIssueID: pr.IID,
			FilePath:  f.Path,
		})
		out := make([]commentRow, 0, len(rows))
		for _, c := range rows {
			cr := commentRow{C: c}
			if c.AuthorUserID.Valid {
				if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, c.AuthorUserID.Int64); err == nil {
					cr.AuthorName = u.Username
				}
			}
			out = append(out, cr)
		}
		threadsByFile[f.Path] = out
	}
	h.renderPullPage(w, r, "files", map[string]any{
		"Files":          files,
		"DiffHTML":       diffHTML,
		"ThreadsByFile":  threadsByFile,
	})
}

// pullChecks renders the Checks tab. Loads suites + runs grouped by
// suite for the PR's head_oid, plus the markdown-rendered output.summary
// for each run.
func (h *Handlers) pullChecks(w http.ResponseWriter, r *http.Request) {
	row, _, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	type runRow struct {
		R              checksdb.CheckRun
		SummaryHTML    template.HTML
		AppSlug        string
	}
	type suiteGroup struct {
		Suite checksdb.CheckSuite
		Runs  []runRow
	}
	groups := []suiteGroup{}
	if pr.HeadOid != "" {
		suites, _ := h.cq.ListCheckSuitesForCommit(r.Context(), h.d.Pool, checksdb.ListCheckSuitesForCommitParams{
			RepoID: row.ID, HeadSha: pr.HeadOid,
		})
		for _, s := range suites {
			runs, _ := h.cq.ListCheckRunsBySuite(r.Context(), h.d.Pool, s.ID)
			rs := make([]runRow, 0, len(runs))
			for _, run := range runs {
				rs = append(rs, runRow{
					R:           run,
					SummaryHTML: renderCheckSummary(run.Output),
					AppSlug:     s.AppSlug,
				})
			}
			groups = append(groups, suiteGroup{Suite: s, Runs: rs})
		}
	}
	h.renderPullPage(w, r, "checks", map[string]any{
		"CheckGroups": groups,
	})
}

// renderCheckSummary parses the JSON `output` blob and renders the
// `summary` field as Markdown via the existing pipeline. Returns empty
// HTML on any error so a malformed payload doesn't break the page.
func renderCheckSummary(raw []byte) template.HTML {
	if len(raw) == 0 {
		return ""
	}
	var o struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(raw, &o); err != nil || o.Summary == "" {
		return ""
	}
	// Summary is bounded by the API's 256 KiB body cap (well under
	// markdown's 1 MiB ceiling). An error here only fires if a
	// structural precondition regresses; the function is a pure
	// presenter so we degrade to empty (the caller is just rendering
	// a tooltip-grade snippet on the PR checks panel).
	html, _ := mdrender.RenderHTML([]byte(o.Summary))
	return template.HTML(html) //nolint:gosec // sanitized by bluemonday UGCPolicy
}

// pullEdit handles POST .../edit
func (h *Handlers) pullEdit(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullCreate)
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
	if err := pulls.EditPR(r.Context(), h.pullsDeps(), pr.IID,
		r.PostFormValue("title"), r.PostFormValue("body")); err != nil {
		h.handlePullWriteError(w, r, err)
		return
	}
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

// pullSetState handles POST .../state.
func (h *Handlers) pullSetState(w http.ResponseWriter, r *http.Request) {
	// Two-pass authorization: read access first, then ActionPullClose
	// with `repo.AuthorUserID = pr.AuthorUserID` set so the policy engine
	// grants author-self-close. Without the second pass, a non-collab
	// fork-PR author couldn't close their own PR (S00-S25 audit, H1).
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	repoRef := policy.NewRepoRefFromRepo(row)
	if pr.IAuthorUserID.Valid {
		repoRef.AuthorUserID = pr.IAuthorUserID.Int64
	}
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionPullClose, repoRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, repoRef, actor), "")
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	state := strings.TrimSpace(r.PostFormValue("state"))
	if err := pulls.SetState(r.Context(), h.pullsDeps(), gitDir, viewer.ID, pr.IID, state); err != nil {
		h.handlePullWriteError(w, r, err)
		return
	}
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

// pullSetReady handles POST .../ready.
func (h *Handlers) pullSetReady(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullCreate)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := pulls.SetReady(r.Context(), h.pullsDeps(), viewer.ID, pr.IID); err != nil {
		h.handlePullWriteError(w, r, err)
		return
	}
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

// pullMerge handles POST .../merge. Performs the merge synchronously
// inside the request so the redirect lands on the merged state. The
// pulls.Merge orchestrator updates repos.default_branch_oid in the
// same tx when the base IS the default branch, since update-ref
// bypasses the push:process hook that normally maintains the column.
func (h *Handlers) pullMerge(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullMerge)
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
	method := strings.TrimSpace(r.PostFormValue("method"))
	if method == "" {
		method = string(row.DefaultMergeMethod)
	}
	if !pulls.AllowedMethod(row, method) {
		h.handlePullWriteError(w, r, pulls.ErrMergeMethodOff)
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if err := pulls.Merge(r.Context(), h.pullsDeps(), pulls.MergeParams{
		PRID:        pr.IID,
		ActorUserID: viewer.ID,
		GitDir:      gitDir,
		Method:      method,
		Subject:     r.PostFormValue("subject"),
		Body:        r.PostFormValue("body"),
	}); err != nil {
		h.handlePullWriteError(w, r, err)
		return
	}
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

func (h *Handlers) handlePullWriteError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, pulls.ErrAlreadyMerged):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "already merged")
	case errors.Is(err, pulls.ErrAlreadyClosed):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "already closed")
	case errors.Is(err, pulls.ErrMergeBlocked):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "merge blocked — branch has conflicts or hasn't been checked yet")
	case errors.Is(err, pulls.ErrMergeMethodOff):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "this merge method is disabled on this repo")
	case errors.Is(err, pulls.ErrConcurrentMerge):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "PR is being merged by another request")
	case errors.Is(err, pulls.ErrBaseNotFound):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "base branch no longer exists")
	case errors.Is(err, pulls.ErrHeadNotFound):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "head branch no longer exists")
	case errors.Is(err, repogit.ErrRefNotFound):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "branch missing")
	default:
		h.d.Logger.WarnContext(r.Context(), "pulls: write", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}

func (h *Handlers) redirectPull(w http.ResponseWriter, r *http.Request, owner, repo string, number int64) {
	http.Redirect(w, r, "/"+owner+"/"+repo+"/pulls/"+strconv.FormatInt(number, 10), http.StatusSeeOther)
}
