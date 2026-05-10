// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

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
		"Title":        "Issues · " + row.Name,
		"Owner":        owner.Username,
		"Repo":         row,
		"Items":        items,
		"State":        stateFilter,
		"OpenCount":    openCount,
		"ClosedCount":  closedCount,
		"Total":        total,
		"Page":         page,
		"PerPage":      perPage,
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"RepoActions":  h.repoActions(r, row.ID),
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": "issues",
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
		"Title":        "New issue · " + row.Name,
		"Owner":        owner.Username,
		"Repo":         row,
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"RepoActions":  h.repoActions(r, row.ID),
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": "issues",
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
	http.Redirect(
		w, r,
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
		"Title":        "New issue · " + row.Name,
		"Owner":        owner,
		"Repo":         row,
		"FormTitle":    title,
		"FormBody":     body,
		"Error":        msg,
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"RepoActions":  h.repoActions(r, row.ID),
		"RepoCounts":   h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":  h.canViewSettings(middleware.CurrentUserFromContext(r.Context())),
		"ActiveSubnav": "issues",
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

	usernames := map[int64]string{}
	usernameFor := func(id int64) string {
		if id == 0 {
			return ""
		}
		if name, ok := usernames[id]; ok {
			return name
		}
		if u, err := h.uq.GetUserByID(r.Context(), h.d.Pool, id); err == nil {
			usernames[id] = u.Username
			return u.Username
		}
		return ""
	}

	authorName := ""
	if issue.AuthorUserID.Valid {
		authorName = usernameFor(issue.AuthorUserID.Int64)
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	pdeps := policy.Deps{Pool: h.d.Pool}
	repoRef := policy.NewRepoRefFromRepo(row)
	stateRef := issueStateRepoRef(row, issue)
	canCommentAction := policy.Can(r.Context(), pdeps, actor, policy.ActionIssueComment, repoRef).Allow
	canCommentThroughLock := policy.HasRoleAtLeast(r.Context(), pdeps, actor, repoRef, policy.RoleTriage)
	canSetIssueState := policy.Can(r.Context(), pdeps, actor, policy.ActionIssueClose, stateRef).Allow
	timeline := h.issueTimelineRows(comments, events, allLabels, milestones, usernameFor)
	viewerAssigned := false
	participants := map[string]struct{}{}
	if authorName != "" {
		participants[authorName] = struct{}{}
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
		if a.UserID == viewer.ID {
			viewerAssigned = true
		}
	}
	participantNames := make([]string, 0, len(participants))
	for name := range participants {
		participantNames = append(participantNames, name)
	}
	sort.Strings(participantNames)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.d.Render.RenderPage(w, r, "repo/issue_view", map[string]any{
		"Title":                 issue.Title + " · " + row.Name,
		"Owner":                 owner.Username,
		"Repo":                  row,
		"Issue":                 issue,
		"AuthorName":            authorName,
		"CommentCount":          len(comments),
		"Timeline":              timeline,
		"Labels":                labels,
		"Assignees":             assignees,
		"Participants":          participantNames,
		"ViewerAssigned":        viewerAssigned,
		"AllLabels":             allLabels,
		"Milestones":            milestones,
		"CanComment":            canCommentAction && (!issue.Locked || canCommentThroughLock),
		"CanSetIssueState":      canSetIssueState,
		"CanEditIssueLabels":    policy.Can(r.Context(), pdeps, actor, policy.ActionIssueLabel, repoRef).Allow,
		"CanEditIssueAssignees": policy.Can(r.Context(), pdeps, actor, policy.ActionIssueAssign, repoRef).Allow,
		"CanEditIssueMilestone": policy.Can(r.Context(), pdeps, actor, policy.ActionIssueLabel, repoRef).Allow,
		"CanLockIssue":          policy.Can(r.Context(), pdeps, actor, policy.ActionIssueClose, repoRef).Allow,
		"CSRFToken":             middleware.CSRFTokenForRequest(r),
		"RepoActions":           h.repoActions(r, row.ID),
		"RepoCounts":            h.subnavCounts(r.Context(), row.ID, row.ForkCount),
		"CanSettings":           h.canViewSettings(viewer),
		"ActiveSubnav":          "issues",
	})
}

type issueTimelineRow struct {
	Type        string
	C           issuesdb.IssueComment
	E           issuesdb.IssueEvent
	CreatedAt   time.Time
	AuthorName  string
	ActorName   string
	Message     string
	LabelName   string
	LabelColor  string
	CommentID   int64
	LinkedState bool
}

func (h *Handlers) issueTimelineRows(
	comments []issuesdb.IssueComment,
	events []issuesdb.IssueEvent,
	labels []issuesdb.Label,
	milestones []issuesdb.Milestone,
	usernameFor func(int64) string,
) []issueTimelineRow {
	labelByID := map[int64]issuesdb.Label{}
	for _, l := range labels {
		labelByID[l.ID] = l
	}
	milestoneByID := map[int64]issuesdb.Milestone{}
	for _, m := range milestones {
		milestoneByID[m.ID] = m
	}
	rows := make([]issueTimelineRow, 0, len(comments)+len(events))
	for _, c := range comments {
		row := issueTimelineRow{Type: "comment", C: c, CreatedAt: c.CreatedAt.Time}
		if c.AuthorUserID.Valid {
			row.AuthorName = usernameFor(c.AuthorUserID.Int64)
		}
		rows = append(rows, row)
	}
	for _, e := range events {
		row := issueTimelineRow{
			Type:      "event",
			E:         e,
			CreatedAt: e.CreatedAt.Time,
			Message:   issueEventMessage(e.Kind),
		}
		if e.ActorUserID.Valid {
			row.ActorName = usernameFor(e.ActorUserID.Int64)
		}
		meta := issueEventMeta(e.Meta)
		if id := metaInt64(meta, "comment_id"); id != 0 {
			row.CommentID = id
			row.LinkedState = e.Kind == "closed" || e.Kind == "reopened"
		}
		if id := metaInt64(meta, "label_id"); id != 0 {
			if l, ok := labelByID[id]; ok {
				row.LabelName = l.Name
				row.LabelColor = l.Color
			}
		}
		if id := metaInt64(meta, "milestone_id"); id != 0 {
			if m, ok := milestoneByID[id]; ok {
				switch e.Kind {
				case "milestoned":
					row.Message = "added this to the " + m.Title + " milestone"
				case "demilestoned":
					row.Message = "removed this from the " + m.Title + " milestone"
				}
			}
		}
		if id := metaInt64(meta, "user_id"); id != 0 {
			if name := usernameFor(id); name != "" {
				switch e.Kind {
				case "assigned":
					row.Message = "assigned " + name
				case "unassigned":
					row.Message = "unassigned " + name
				}
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	return rows
}

func issueEventMeta(raw []byte) map[string]any {
	var out map[string]any
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func metaInt64(meta map[string]any, key string) int64 {
	if meta == nil {
		return 0
	}
	switch v := meta[key].(type) {
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func issueEventMessage(kind string) string {
	switch kind {
	case "closed":
		return "closed this issue"
	case "reopened":
		return "reopened this issue"
	case "locked":
		return "locked this conversation"
	case "unlocked":
		return "unlocked this conversation"
	case "labeled":
		return "added a label"
	case "unlabeled":
		return "removed a label"
	case "milestoned":
		return "added this to a milestone"
	case "demilestoned":
		return "removed this from a milestone"
	case "assigned":
		return "assigned a user"
	case "unassigned":
		return "unassigned a user"
	case "referenced":
		return "referenced this issue"
	default:
		return kind
	}
}

func issueStateRepoRef(row reposdb.Repo, issue issuesdb.Issue) policy.RepoRef {
	ref := policy.NewRepoRefFromRepo(row)
	if issue.AuthorUserID.Valid {
		ref.AuthorUserID = issue.AuthorUserID.Int64
	}
	return ref
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
	body := strings.TrimSpace(r.PostFormValue("body"))
	state := strings.TrimSpace(r.PostFormValue("state"))
	reason := strings.TrimSpace(r.PostFormValue("reason"))

	// IsCollab is the locked-issue bypass: triage+ on the repo can comment
	// past a `locked=true` flag (the gate exists to silence drive-by
	// posters). We resolve the *real* role via the policy package — owner
	// is implicit admin, and any explicit collaborator with role >= triage
	// passes. Read fails (DB miss, unknown role) fail closed via RoleNone.
	actor := policy.UserActor(viewer.ID, viewer.Username, viewer.IsSuspended, false)
	isCollab := policy.HasRoleAtLeast(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.NewRepoRefFromRepo(row), policy.RoleTriage)

	var commentID int64
	if body != "" {
		c, err := issues.AddComment(r.Context(), h.issuesDeps(), issues.CommentCreateParams{
			IssueID:      issue.ID,
			AuthorUserID: viewer.ID,
			Body:         body,
			IsCollab:     isCollab,
		})
		if err != nil {
			h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
			return
		}
		commentID = c.ID
	} else if state == "" {
		h.handleIssueWriteError(w, r, owner.Username, row, issue, issues.ErrEmptyComment)
		return
	}
	if state != "" {
		stateRef := issueStateRepoRef(row, issue)
		if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionIssueClose, stateRef); !dec.Allow {
			h.d.Render.HTTPError(w, r, policy.Maybe404(dec, stateRef, actor), "")
			return
		}
		var err error
		if commentID != 0 {
			err = issues.SetStateWithComment(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, state, reason, commentID)
		} else {
			err = issues.SetState(r.Context(), h.issuesDeps(), viewer.ID, issue.ID, state, reason)
		}
		if err != nil {
			h.handleIssueWriteError(w, r, owner.Username, row, issue, err)
			return
		}
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
	stateRef := issueStateRepoRef(row, issue)
	if dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionIssueClose, stateRef); !dec.Allow {
		h.d.Render.HTTPError(w, r, policy.Maybe404(dec, stateRef, actor), "")
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
	case errors.Is(err, issues.ErrEmptyComment):
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "comment body required")
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
