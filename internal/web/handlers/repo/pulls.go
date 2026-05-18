// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/billing"
	checksdomain "github.com/tenseleyFlow/shithub/internal/checks"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	mdrender "github.com/tenseleyFlow/shithub/internal/markdown"
	"github.com/tenseleyFlow/shithub/internal/pulls"
	pullsdb "github.com/tenseleyFlow/shithub/internal/pulls/sqlc"
	repogit "github.com/tenseleyFlow/shithub/internal/repos/git"
	"github.com/tenseleyFlow/shithub/internal/repos/identity"
	"github.com/tenseleyFlow/shithub/internal/repos/protection"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type pullPageStats struct {
	Comments         int
	Commits          int
	Files            int
	Checks           int
	SuccessfulChecks int
	PendingChecks    int
	FailedChecks     int
	CheckState       string
	CheckSummary     codeCommitCheckSummary
}

type pullCheckRunView struct {
	R           checksdb.CheckRun
	SummaryHTML template.HTML
	AppSlug     string
	DetailsHref string
	RerunHref   string
	CanRerun    bool
	StateClass  string
	StateIcon   string
}

type pullCheckSuiteView struct {
	Suite checksdb.CheckSuite
	Runs  []pullCheckRunView
}

type pullRequiredChecksView struct {
	HasRequired bool
	Names       []string
	Missing     []string
	Satisfied   bool
	Reason      string
	Error       string
}

type pullFileView struct {
	F      pullsdb.PullRequestFile
	Anchor string
	Dir    string
	Name   string
}

type pullCommitView struct {
	C           pullsdb.PullRequestCommit
	Author      identity.Resolved
	ShortSHA    string
	When        time.Time
	HasWhen     bool
	AuthorLabel string
}

type pullCommitGroup struct {
	Title   string
	Commits []pullCommitView
}

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
	r.Get("/{owner}/{repo}/pulls/{number}.diff", h.pullRawDiff)
	r.Get("/{owner}/{repo}/pulls/{number}.patch", h.pullRawDiff)
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
		r.Post("/{owner}/{repo}/pulls/{number}/delete-branch", h.pullDeleteHeadBranch)
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
		Checks     codeCommitCheckSummary
	}
	headSHAs := make([]string, 0, len(rows))
	for _, lr := range rows {
		headSHAs = append(headSHAs, lr.HeadOid)
	}
	checkSummaries := h.codeCommitCheckSummaries(r.Context(), owner.Username, row.Name, row.ID, headSHAs)
	items := make([]listItem, 0, len(rows))
	for _, lr := range rows {
		it := listItem{Row: lr, Checks: checkSummaries[lr.HeadOid]}
		if lr.AuthorUserID.Valid {
			if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, lr.AuthorUserID.Int64); err == nil {
				it.AuthorName = u.Username
			}
		}
		items = append(items, it)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/pulls_list", map[string]any{
		"Title":        "Pull requests · " + row.Name,
		"Owner":        owner.Username,
		"Repo":         row,
		"Items":        items,
		"State":        state,
		"OpenCount":    openCount,
		"ClosedCount":  closedCount,
		"Page":         page,
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": "pulls",
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
	if strings.TrimSpace(head) == "" {
		http.Redirect(w, r, "/"+owner.Username+"/"+row.Name+"/compare", http.StatusSeeOther)
		return
	}
	h.renderPullNewForm(w, r, owner.Username, row, pullNewFormOptions{
		Base: base,
		Head: head,
	})
}

type pullNewFormOptions struct {
	Base      string
	Head      string
	FormTitle string
	FormBody  string
	Error     string
	Status    int
}

func (h *Handlers) renderPullNewForm(w http.ResponseWriter, r *http.Request, owner string, row reposdb.Repo, opts pullNewFormOptions) {
	gitDir, err := h.d.RepoFS.RepoPath(owner, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	base := strings.TrimSpace(opts.Base)
	if base == "" {
		base = row.DefaultBranch
	}
	head := strings.TrimSpace(opts.Head)
	if head == "" {
		head = row.DefaultBranch
	}
	state := h.buildCompareState(r, owner, row, gitDir, base, head, true, compareMenuTargetPullNew)
	formTitle := opts.FormTitle
	if strings.TrimSpace(formTitle) == "" && opts.Error == "" {
		formTitle = defaultPullTitle(state.Head, state.Commits)
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	status := opts.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	_ = h.d.Render.RenderPage(w, r, "repo/pull_new", mergePageData(
		h.repoPageChrome(r, owner, row, "pulls"),
		map[string]any{
			"Title":               "Open a pull request · " + row.Name,
			"UseCompareJS":        true,
			"UseCommentEditor":    true,
			"CommentEditorConfig": commentEditorConfigJSON(h.pullNewCommentEditorConfig(r.Context(), row, viewer)),
			"Viewer":              viewer,
			"ViewerAvatarURL":     commentEditorAvatarURL(viewer.Username),
			"Error":               opts.Error,
			"FormTitle":           formTitle,
			"FormBody":            opts.FormBody,
			"Base":                state.Base,
			"Head":                state.Head,
			"HasSelection":        state.HasSelection,
			"SameRef":             state.SameRef,
			"NotFound":            state.NotFound,
			"CommitsErr":          state.CommitsErr,
			"NoCommits":           state.NoCommits,
			"Ahead":               state.Ahead,
			"Behind":              state.Behind,
			"Commits":             state.Commits,
			"DiffHTML":            state.DiffHTML,
			"Stats":               state.Stats,
			"MergeState":          state.MergeState,
			"CanOpenPull":         state.CanOpenPull,
			"CanCreatePull":       state.CanOpenPull && !state.NotFound && !state.CommitsErr,
			"PullNewHref":         state.PullNewHref,
			"BaseMenu":            state.BaseMenu,
			"HeadMenu":            state.HeadMenu,
		},
	))
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
	// Auto-watch on first involvement (S26): subscribe the PR author
	// at `participating` so notifications fan-out routes future
	// thread events to them.
	_ = social.AutoWatchOnInvolvement(r.Context(), h.socialDeps(), viewer.ID, row.ID)
	// Kick off the mergeability probe right away.
	if _, err := worker.Enqueue(r.Context(), h.d.Pool, worker.KindPRMergeability,
		map[string]any{"pr_id": res.PullRequest.IssueID}, worker.EnqueueOptions{}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "pulls: enqueue mergeability", "error", err)
	}
	_ = worker.Notify(r.Context(), h.d.Pool)
	http.Redirect(
		w, r,
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
	h.renderPullNewForm(w, r, owner, row, pullNewFormOptions{
		Base:      r.PostFormValue("base"),
		Head:      r.PostFormValue("head"),
		FormTitle: r.PostFormValue("title"),
		FormBody:  r.PostFormValue("body"),
		Error:     msg,
		Status:    http.StatusBadRequest,
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
	mergedByName := ""
	if pr.MergedByUserID.Valid {
		if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, pr.MergedByUserID.Int64); err == nil {
			mergedByName = u.Username
		}
	}
	if pr.IState == pullsdb.IssueStateOpen &&
		pr.MergeableState == pullsdb.PrMergeableStateUnknown &&
		pr.BaseOid != "" && pr.HeadOid != "" {
		h.kickMergeability(r, pr.IID)
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := viewer.PolicyActor()
	pdeps := policy.Deps{Pool: h.d.Pool}
	repoRef := policy.NewRepoRefFromRepo(row)
	stateRef := repoRef
	if pr.IAuthorUserID.Valid {
		stateRef.AuthorUserID = pr.IAuthorUserID.Int64
	}
	canReviewPull := policy.Can(r.Context(), pdeps, actor, policy.ActionPullReview, repoRef).Allow
	canMergePull := policy.Can(r.Context(), pdeps, actor, policy.ActionPullMerge, repoRef).Allow
	canSetPullState := policy.Can(r.Context(), pdeps, actor, policy.ActionPullClose, stateRef).Allow
	canReadyPull := policy.Can(r.Context(), pdeps, actor, policy.ActionPullCreate, repoRef).Allow
	canRerunChecks := policy.Can(r.Context(), pdeps, actor, policy.ActionRepoWrite, repoRef).Allow
	headOwner := owner.Username
	if pr.HeadRepoID != 0 {
		if headRepo, err := h.rq.GetRepoOwnerUsernameByID(r.Context(), h.d.Pool, pr.HeadRepoID); err == nil {
			if ownerName := repoOwnerName(headRepo.OwnerUsername); ownerName != "" {
				headOwner = ownerName
			}
		}
	}
	headBranchExists := false
	if pr.HeadRepoID == row.ID && pr.HeadRef != "" && pr.HeadRef != row.DefaultBranch {
		if gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name); err == nil {
			if _, err := repogit.ResolveRefOID(r.Context(), gitDir, "refs/heads/"+pr.HeadRef); err == nil {
				headBranchExists = true
			}
		}
	}
	defaultMethod := string(row.DefaultMergeMethod)
	if defaultMethod == "" {
		defaultMethod = "merge"
	}
	checkGroups := h.pullCheckGroups(r.Context(), owner.Username, row.Name, row.ID, pr.HeadOid, canRerunChecks)
	stats := h.pullStats(r.Context(), pr, owner.Username, row.Name, checkGroups)
	requiredChecks := h.pullRequiredChecksView(r.Context(), row.ID, pr.BaseRef, pr.HeadOid)
	data := map[string]any{
		"Title":                 "#" + strconv.FormatInt(pr.INumber, 10) + " " + pr.ITitle + " · " + row.Name,
		"Owner":                 owner.Username,
		"Repo":                  row,
		"PR":                    pr,
		"AuthorName":            authorName,
		"MergedByName":          mergedByName,
		"Tab":                   tab,
		"PullStats":             stats,
		"CheckGroups":           checkGroups,
		"RequiredChecks":        requiredChecks,
		"CSRFToken":             middleware.CSRFTokenForRequest(r),
		"RepoActions":           h.repoActions(r, row.ID),
		"RepoCounts":            h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":           h.canViewSettings(viewer),
		"ActiveSubnav":          "pulls",
		"UsePullViewJS":         true,
		"MergeDefaultMethod":    defaultMethod,
		"MergeFormSubject":      defaultMergeSubject(pr, headOwner),
		"MergeFormBody":         strings.TrimSpace(pr.ITitle),
		"MergeAuthorLine":       h.mergeAuthorLine(r.Context(), viewer),
		"CanDeleteHeadBranch":   canMergePull && pr.MergedAt.Valid && pr.HeadRepoID == row.ID && pr.HeadRef != row.DefaultBranch && headBranchExists,
		"HeadBranchAlreadyGone": pr.MergedAt.Valid && pr.HeadRepoID == row.ID && pr.HeadRef != row.DefaultBranch && !headBranchExists,
	}
	data["CanReviewPull"] = canReviewPull
	data["CanMergePull"] = canMergePull
	data["CanSetPullState"] = canSetPullState
	data["CanReadyPull"] = canReadyPull
	for k, v := range extras {
		data[k] = v
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/pull_view", data)
}

func repoOwnerName(raw interface{}) string {
	switch v := raw.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func defaultMergeSubject(pr pullsdb.GetPullRequestByRepoAndNumberRow, headOwner string) string {
	head := strings.TrimSpace(headOwner)
	if head == "" {
		head = "head"
	}
	if pr.HeadRef != "" {
		head += "/" + pr.HeadRef
	}
	return "Merge pull request #" + strconv.FormatInt(pr.INumber, 10) + " from " + head
}

func (h *Handlers) mergeAuthorLine(ctx context.Context, viewer middleware.CurrentUser) string {
	if viewer.IsAnonymous() {
		return ""
	}
	email := viewer.Username + "@noreply.shithub.local"
	if u, err := h.uq.GetUserByID(ctx, h.d.Pool, viewer.ID); err == nil && u.PrimaryEmailID.Valid {
		if em, err := h.uq.GetUserEmailByID(ctx, h.d.Pool, u.PrimaryEmailID.Int64); err == nil && em.Verified {
			email = string(em.Email)
		}
	}
	return "This commit will be authored by " + email + "."
}

func (h *Handlers) pullStats(ctx context.Context, pr pullsdb.GetPullRequestByRepoAndNumberRow, owner, repoName string, checkGroups []pullCheckSuiteView) pullPageStats {
	stats := pullPageStats{CheckState: "none"}
	if comments, err := h.iq.ListIssueComments(ctx, h.d.Pool, pr.IID); err == nil {
		stats.Comments = len(comments)
	}
	if commits, err := h.pq.ListPullRequestCommits(ctx, h.d.Pool, pr.IID); err == nil {
		stats.Commits = len(commits)
	}
	if files, err := h.pq.ListPullRequestFiles(ctx, h.d.Pool, pr.IID); err == nil {
		stats.Files = len(files)
	}
	latestRuns := latestPullCheckRuns(checkGroups)
	for _, run := range latestRuns {
		stats.Checks++
		if run.Conclusion.Valid {
			switch run.Conclusion.CheckConclusion {
			case checksdb.CheckConclusionSuccess, checksdb.CheckConclusionSkipped, checksdb.CheckConclusionNeutral:
				stats.SuccessfulChecks++
			case checksdb.CheckConclusionFailure, checksdb.CheckConclusionCancelled, checksdb.CheckConclusionTimedOut, checksdb.CheckConclusionActionRequired, checksdb.CheckConclusionStale:
				stats.FailedChecks++
			default:
				stats.PendingChecks++
			}
			continue
		}
		stats.PendingChecks++
	}
	switch {
	case stats.Checks == 0:
		stats.CheckState = "none"
	case stats.FailedChecks > 0:
		stats.CheckState = "failure"
	case stats.PendingChecks > 0:
		stats.CheckState = "pending"
	default:
		stats.CheckState = "success"
	}
	stats.CheckSummary = summarizeCodeCommitChecks(latestRuns)
	if stats.CheckSummary.Show {
		stats.CheckSummary.Href = codeCheckSummaryHref(owner, repoName, latestRuns)
	}
	return stats
}

func latestPullCheckRuns(checkGroups []pullCheckSuiteView) []checksdb.CheckRun {
	byName := map[string]checksdb.CheckRun{}
	for _, group := range checkGroups {
		for _, run := range group.Runs {
			cur, ok := byName[run.R.Name]
			if !ok || checkRunNewer(run.R, cur) {
				byName[run.R.Name] = run.R
			}
		}
	}
	out := make([]checksdb.CheckRun, 0, len(byName))
	for _, run := range byName {
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func checkRunNewer(a, b checksdb.CheckRun) bool {
	if a.CreatedAt.Valid && b.CreatedAt.Valid && !a.CreatedAt.Time.Equal(b.CreatedAt.Time) {
		return a.CreatedAt.Time.After(b.CreatedAt.Time)
	}
	return a.ID > b.ID
}

func (h *Handlers) pullCheckGroups(ctx context.Context, owner, repoName string, repoID int64, headOID string, canRerun bool) []pullCheckSuiteView {
	groups := []pullCheckSuiteView{}
	if headOID == "" {
		return groups
	}
	suites, _ := h.cq.ListCheckSuitesForCommit(ctx, h.d.Pool, checksdb.ListCheckSuitesForCommitParams{
		RepoID: repoID, HeadSha: headOID,
	})
	for _, suite := range suites {
		runs, _ := h.cq.ListCheckRunsBySuite(ctx, h.d.Pool, suite.ID)
		rs := make([]pullCheckRunView, 0, len(runs))
		for _, run := range runs {
			detailsHref := sameRepoLocalDetailsHref(owner, repoName, run.DetailsUrl)
			rerunHref, isActionsRun := localActionsRunRerunHref(owner, repoName, run.DetailsUrl)
			isShithubActions := suite.AppSlug == "shithub-actions"
			canRerunRun := canRerun && isShithubActions && isActionsRun && run.Status == checksdb.CheckStatusCompleted
			if !canRerunRun {
				rerunHref = ""
			}
			rs = append(rs, pullCheckRunView{
				R:           run,
				SummaryHTML: renderCheckSummary(run.Output),
				AppSlug:     suite.AppSlug,
				DetailsHref: detailsHref,
				RerunHref:   rerunHref,
				CanRerun:    canRerunRun,
				StateClass:  pullCheckRunStateClass(run),
				StateIcon:   pullCheckRunStateIcon(run),
			})
		}
		groups = append(groups, pullCheckSuiteView{Suite: suite, Runs: rs})
	}
	return groups
}

func pullCheckRunStateClass(run checksdb.CheckRun) string {
	if run.Status != checksdb.CheckStatusCompleted || !run.Conclusion.Valid {
		return "pending"
	}
	switch run.Conclusion.CheckConclusion {
	case checksdb.CheckConclusionSuccess:
		return "success"
	case checksdb.CheckConclusionFailure, checksdb.CheckConclusionTimedOut, checksdb.CheckConclusionActionRequired:
		return "failure"
	case checksdb.CheckConclusionCancelled:
		return "cancelled"
	case checksdb.CheckConclusionSkipped:
		return "skipped"
	case checksdb.CheckConclusionNeutral, checksdb.CheckConclusionStale:
		return "neutral"
	default:
		return "neutral"
	}
}

func pullCheckRunStateIcon(run checksdb.CheckRun) string {
	if run.Status != checksdb.CheckStatusCompleted || !run.Conclusion.Valid {
		return "dot-fill"
	}
	switch run.Conclusion.CheckConclusion {
	case checksdb.CheckConclusionSuccess:
		return "check-circle"
	case checksdb.CheckConclusionFailure, checksdb.CheckConclusionTimedOut, checksdb.CheckConclusionActionRequired:
		return "x-circle"
	case checksdb.CheckConclusionCancelled:
		return "stop"
	case checksdb.CheckConclusionSkipped:
		return "dash"
	default:
		return "dot-fill"
	}
}

func (h *Handlers) pullRequiredChecksView(ctx context.Context, repoID int64, baseRef, headOID string) pullRequiredChecksView {
	rules, err := h.rq.ListBranchProtectionRules(ctx, h.d.Pool, repoID)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "pulls: ListBranchProtectionRules", "repo_id", repoID, "error", err)
		return pullRequiredChecksView{Error: "Required check rules could not be loaded."}
	}
	rule, ok := protection.MatchLongestRule(rules, baseRef)
	if !ok || len(rule.StatusChecksRequired) == 0 {
		return pullRequiredChecksView{Satisfied: true}
	}
	names := append([]string(nil), rule.StatusChecksRequired...)
	result, err := checksdomain.EvaluateRequiredChecks(ctx, h.d.Pool, checksdomain.GateInputs{
		RepoID:        repoID,
		HeadSHA:       headOID,
		RequiredNames: names,
	})
	if err != nil {
		h.d.Logger.WarnContext(ctx, "pulls: EvaluateRequiredChecks", "repo_id", repoID, "head_sha", headOID, "error", err)
		return pullRequiredChecksView{
			HasRequired: true,
			Names:       names,
			Error:       "Required checks could not be evaluated.",
		}
	}
	return pullRequiredChecksView{
		HasRequired: true,
		Names:       names,
		Missing:     append([]string(nil), result.Missing...),
		Satisfied:   result.Satisfied,
		Reason:      result.Reason,
	}
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
	labels, _ := h.iq.ListLabelsOnIssue(r.Context(), h.d.Pool, pr.IID)
	assignees, _ := h.iq.ListIssueAssignees(r.Context(), h.d.Pool, pr.IID)
	allLabels, _ := h.iq.ListLabels(r.Context(), h.d.Pool, row.ID)
	milestones, _ := h.iq.ListMilestones(r.Context(), h.d.Pool, row.ID)
	issueProjects, projectOptions := h.issueProjectData(r.Context(), row, pr.IID)
	reviews, _ := h.pq.ListPRReviews(r.Context(), h.d.Pool, pr.IID)
	requests, _ := h.pq.ListPRReviewRequests(r.Context(), h.d.Pool, pr.IID)

	// PRO-EXT01-04c: see issues.go::issueView for the same cache shape;
	// the Pro-username set is what the template uses to render pills
	// next to every author/actor handle on the page.
	//
	// PRO-EXT_SR2-12 (audit H5): pre-fetch the participant set so the
	// closure becomes a map lookup instead of one GetUserByID per
	// distinct participant.
	usernames := map[int64]string{}
	proUsernames := map[string]bool{}
	participantIDs := collectPullParticipantIDs(pr, comments, events, assignees, reviews, requests)
	if len(participantIDs) > 0 {
		preloaded, err := h.uq.ListUsersByIDs(r.Context(), h.d.Pool, participantIDs)
		if err == nil {
			for _, u := range preloaded {
				usernames[u.ID] = u.Username
				if billing.IsProUserPlan(billing.UserPlan(u.Plan)) {
					proUsernames[u.Username] = true
				}
			}
		}
	}
	usernameFor := func(id int64) string {
		if id == 0 {
			return ""
		}
		if name, ok := usernames[id]; ok {
			return name
		}
		if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, id); err == nil {
			usernames[id] = u.Username
			if billing.IsProUserPlan(billing.UserPlan(u.Plan)) {
				proUsernames[u.Username] = true
			}
			return u.Username
		}
		return ""
	}
	timeline := h.issueTimelineRows(comments, events, allLabels, milestones, usernameFor)

	// Reviews + reviewer requests for the Conversation sidebar.
	type reviewRow struct {
		R          pullsdb.PrReview
		AuthorName string
	}
	rs := make([]reviewRow, 0, len(reviews))
	for _, rv := range reviews {
		rr := reviewRow{R: rv}
		if rv.AuthorUserID.Valid {
			// PRO-EXT01-04c: usernameFor seeds the Pro-username set so
			// reviewers (rendered in the Conversation sidebar) get a
			// pill next to their handle.
			rr.AuthorName = usernameFor(rv.AuthorUserID.Int64)
		}
		rs = append(rs, rr)
	}
	requestTargets, _ := h.pq.ListPRReviewRequestTargets(r.Context(), h.d.Pool, pr.IID)
	type reqRow struct {
		R        pullsdb.ListPRReviewRequestTargetsRow
		Username string
		TeamSlug string
		OrgSlug  string
	}
	reqs := make([]reqRow, 0, len(requestTargets))
	for _, rq := range requestTargets {
		rr := reqRow{R: rq}
		if rq.RequestedUserID.Valid {
			if rq.RequestedUsername.Valid {
				rr.Username = rq.RequestedUsername.String
				_ = usernameFor(rq.RequestedUserID.Int64)
			} else {
				rr.Username = usernameFor(rq.RequestedUserID.Int64)
			}
		}
		if rq.RequestedTeamID.Valid {
			if rq.RequestedTeamSlug.Valid {
				rr.TeamSlug = rq.RequestedTeamSlug.String
			}
			if rq.RequestedTeamOrgSlug.Valid {
				rr.OrgSlug = rq.RequestedTeamOrgSlug.String
			}
		}
		reqs = append(reqs, rr)
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := viewer.PolicyActor()
	pdeps := policy.Deps{Pool: h.d.Pool}
	repoRef := policy.NewRepoRefFromRepo(row)
	canCommentAction := policy.Can(r.Context(), pdeps, actor, policy.ActionIssueComment, repoRef).Allow
	canCommentThroughLock := policy.HasRoleAtLeast(r.Context(), pdeps, actor, repoRef, policy.RoleTriage)
	viewerAssigned := false
	participants := map[string]struct{}{}
	if pr.IAuthorUserID.Valid {
		if name := usernameFor(pr.IAuthorUserID.Int64); name != "" {
			participants[name] = struct{}{}
		}
	}
	for _, c := range comments {
		if c.AuthorUserID.Valid {
			if name := usernameFor(c.AuthorUserID.Int64); name != "" {
				participants[name] = struct{}{}
			}
		}
	}
	for _, a := range assignees {
		participants[a.Username] = struct{}{}
		_ = usernameFor(a.UserID)
		if a.UserID == viewer.ID {
			viewerAssigned = true
		}
	}
	participantNames := make([]string, 0, len(participants))
	for name := range participants {
		participantNames = append(participantNames, name)
	}
	sort.Strings(participantNames)
	h.renderPullPage(w, r, "conversation", map[string]any{
		"Timeline":              timeline,
		"Events":                events,
		"Labels":                labels,
		"Assignees":             assignees,
		"Participants":          participantNames,
		"ProUsernames":          proUsernames,
		"ViewerAssigned":        viewerAssigned,
		"AllLabels":             allLabels,
		"Milestones":            milestones,
		"Projects":              issueProjects,
		"ProjectOptions":        projectOptions,
		"Reviews":               rs,
		"ReviewRequests":        reqs,
		"CanComment":            canCommentAction && (!pr.ILocked || canCommentThroughLock),
		"CanEditIssueLabels":    policy.Can(r.Context(), pdeps, actor, policy.ActionIssueLabel, repoRef).Allow,
		"CanEditIssueAssignees": policy.Can(r.Context(), pdeps, actor, policy.ActionIssueAssign, repoRef).Allow,
		"CanEditIssueMilestone": policy.Can(r.Context(), pdeps, actor, policy.ActionIssueLabel, repoRef).Allow,
		"CanEditIssueProjects":  h.canEditIssueProjects(r.Context(), row, chi.URLParam(r, "owner"), actor),
		"CanLockIssue":          policy.Can(r.Context(), pdeps, actor, policy.ActionIssueClose, repoRef).Allow,
		"UseCommentEditor":      true,
		"ViewerAvatarURL":       commentEditorAvatarURL(viewer.Username),
		"CommentEditorConfig":   commentEditorConfigJSON(h.pullCommentEditorConfig(r.Context(), row, pr, viewer, comments, assignees, reviews, requests)),
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
	commitGroups := pullCommitGroups(r.Context(), commits, identity.New(h.d.Pool))
	h.renderPullPage(w, r, "commits", map[string]any{
		"CommitGroups": commitGroups,
	})
}

func pullCommitGroups(ctx context.Context, commits []pullsdb.PullRequestCommit, resolver *identity.Resolver) []pullCommitGroup {
	groups := make([]pullCommitGroup, 0, 2)
	for _, commit := range commits {
		when, hasWhen := pullCommitWhen(commit)
		title := "Commits"
		if hasWhen {
			title = "Commits on " + when.Format("January 2, 2006")
		}
		if len(groups) == 0 || groups[len(groups)-1].Title != title {
			groups = append(groups, pullCommitGroup{Title: title})
		}
		author := identity.Resolved{}
		if resolver != nil {
			author = resolver.Resolve(ctx, commit.AuthorEmail)
		}
		authorLabel := commit.AuthorName
		if author.User && author.DisplayName != "" {
			authorLabel = author.DisplayName
		} else if author.User {
			authorLabel = author.Username
		}
		shortSHA := commit.Sha
		if len(shortSHA) > 7 {
			shortSHA = shortSHA[:7]
		}
		groups[len(groups)-1].Commits = append(groups[len(groups)-1].Commits, pullCommitView{
			C:           commit,
			Author:      author,
			ShortSHA:    shortSHA,
			When:        when,
			HasWhen:     hasWhen,
			AuthorLabel: authorLabel,
		})
	}
	return groups
}

func pullCommitWhen(commit pullsdb.PullRequestCommit) (time.Time, bool) {
	if commit.CommittedAt.Valid {
		return commit.CommittedAt.Time, true
	}
	if commit.AuthoredAt.Valid {
		return commit.AuthoredAt.Time, true
	}
	return time.Time{}, false
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
	fileViews := make([]pullFileView, 0, len(files))
	for _, f := range files {
		label := pullFileLabel(f)
		dir, name := splitPullFilePath(f.Path)
		fileViews = append(fileViews, pullFileView{
			F:      f,
			Anchor: pullFileAnchor(label),
			Dir:    dir,
			Name:   name,
		})
	}
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
		"Files":         fileViews,
		"DiffHTML":      diffHTML,
		"ThreadsByFile": threadsByFile,
	})
}

func pullFileLabel(f pullsdb.PullRequestFile) string {
	if f.OldPath.Valid && f.OldPath.String != "" && f.OldPath.String != f.Path {
		return f.OldPath.String + " → " + f.Path
	}
	return f.Path
}

func splitPullFilePath(p string) (string, string) {
	idx := strings.LastIndex(p, "/")
	if idx < 0 {
		return "", p
	}
	return p[:idx], p[idx+1:]
}

func pullFileAnchor(p string) string {
	var b strings.Builder
	b.WriteString("diff-")
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (h *Handlers) pullRawDiff(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionPullRead)
	if !ok {
		return
	}
	pr, ok := h.loadPullByNumber(w, r, row.ID)
	if !ok {
		return
	}
	if pr.BaseOid == "" || pr.HeadOid == "" {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	patch, err := compareSourceMergeBase(r, gitDir, pr.BaseOid, pr.HeadOid)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "pulls: raw diff", "error", err, "repo_id", row.ID, "pr", pr.INumber)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	ext := ".diff"
	if strings.HasSuffix(r.URL.Path, ".patch") {
		ext = ".patch"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline; filename=\""+row.Name+"-"+strconv.FormatInt(pr.INumber, 10)+ext+"\"")
	_, _ = w.Write(patch) // #nosec G705 -- git diff bytes are served as text/plain with nosniff, not HTML.
}

// pullChecks renders the Checks tab. Loads suites + runs grouped by
// suite for the PR's head_oid, plus the markdown-rendered output.summary
// for each run.
func (h *Handlers) pullChecks(w http.ResponseWriter, r *http.Request) {
	h.renderPullPage(w, r, "checks", nil)
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
	actor := viewer.PolicyActor()
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

func (h *Handlers) pullDeleteHeadBranch(w http.ResponseWriter, r *http.Request) {
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
	if !pr.MergedAt.Valid || pr.HeadRepoID != row.ID || strings.TrimSpace(pr.HeadRef) == "" || pr.HeadRef == row.DefaultBranch {
		h.d.Render.HTTPError(w, r, http.StatusConflict, "head branch cannot be deleted from this pull request")
		return
	}
	gitDir, err := h.d.RepoFS.RepoPath(owner.Username, row.Name)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if err := repogit.DeleteBranch(r.Context(), gitDir, pr.HeadRef, pr.HeadOid); err != nil {
		if errors.Is(err, repogit.ErrRefNotFound) {
			h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
			return
		}
		if errors.Is(err, repogit.ErrRefRaced) {
			h.d.Render.HTTPError(w, r, http.StatusConflict, "branch moved after this pull request was merged")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "pulls: delete head branch", "error", err, "repo_id", row.ID, "pr", pr.INumber, "branch", pr.HeadRef)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.redirectPull(w, r, owner.Username, row.Name, pr.INumber)
}

// collectPullParticipantIDs builds the distinct set of user IDs we
// know we'll need a username + plan for when rendering a PR view:
// PR author, merger, every comment author, every event actor, every
// assignee, every review author, every review-request recipient.
// Mirrors collectIssueParticipantIDs in issues.go. PRO-EXT_SR2-12.
func collectPullParticipantIDs(
	pr pullsdb.GetPullRequestByRepoAndNumberRow,
	comments []issuesdb.IssueComment,
	events []issuesdb.IssueEvent,
	assignees []issuesdb.ListIssueAssigneesRow,
	reviews []pullsdb.PrReview,
	requests []pullsdb.PrReviewRequest,
) []int64 {
	seen := make(map[int64]struct{}, len(comments)+len(events)+len(assignees)+len(reviews)+len(requests)+2)
	add := func(id int64) {
		if id == 0 {
			return
		}
		seen[id] = struct{}{}
	}
	if pr.IAuthorUserID.Valid {
		add(pr.IAuthorUserID.Int64)
	}
	if pr.MergedByUserID.Valid {
		add(pr.MergedByUserID.Int64)
	}
	for _, c := range comments {
		if c.AuthorUserID.Valid {
			add(c.AuthorUserID.Int64)
		}
	}
	for _, e := range events {
		if e.ActorUserID.Valid {
			add(e.ActorUserID.Int64)
		}
	}
	for _, a := range assignees {
		add(a.UserID)
	}
	for _, rv := range reviews {
		if rv.AuthorUserID.Valid {
			add(rv.AuthorUserID.Int64)
		}
	}
	for _, rq := range requests {
		if rq.RequestedUserID.Valid {
			add(rq.RequestedUserID.Int64)
		}
	}
	out := make([]int64, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	return out
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
