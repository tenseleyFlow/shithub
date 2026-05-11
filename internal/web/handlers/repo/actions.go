// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const actionsRunsPageSize = int32(20)

type actionsWorkflowView struct {
	File   string
	Name   string
	Count  int64
	Href   string
	Active bool
}

type actionsListRunView struct {
	ID            int64
	RunIndex      int64
	WorkflowFile  string
	WorkflowName  string
	Title         string
	HeadSha       string
	HeadShaShort  string
	HeadRef       string
	Event         string
	EventLabel    string
	ActorUsername string
	StateText     string
	StateClass    string
	StateIcon     string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Duration      string
	Href          string
}

type actionsListFilters struct {
	Workflow   string
	Branch     string
	Event      string
	Status     string
	Conclusion string
	Actor      string
	Page       int32
	HasAny     bool
}

type actionsFilterOption struct {
	Value    string
	Label    string
	Selected bool
}

type actionsPaginationView struct {
	Page       int32
	PageSize   int32
	Total      int64
	Start      int64
	End        int64
	HasPrev    bool
	HasNext    bool
	PrevHref   string
	NextHref   string
	ResultText string
}

type actionsSuiteView struct {
	ID                 int64
	AppSlug            string
	Title              string
	HeadSha            string
	HeadShaShort       string
	PullNumber         int64
	PullAuthorUsername string
	HeadRef            string
	BaseRef            string
	RunCount           int
	StateText          string
	StateClass         string
	StateIcon          string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Duration           string
	Runs               []actionsRunView
	AnnotationCount    int
}

type actionsRunView struct {
	ID          int64
	Name        string
	StateText   string
	StateClass  string
	StateIcon   string
	Duration    string
	CompletedAt time.Time
	DetailsURL  string
	SummaryHTML template.HTML
}

func (h *Handlers) repoTabActions(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}

	filters := actionsListFiltersFromRequest(r)
	q := actionsdb.New()
	params := workflowRunListParams(row.ID, filters)
	params.PageLimit = actionsRunsPageSize
	params.PageOffset = (filters.Page - 1) * actionsRunsPageSize

	runs, err := q.ListWorkflowRunsForRepo(r.Context(), h.d.Pool, params)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list workflow runs", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	filteredCount, err := q.CountWorkflowRunsForRepo(r.Context(), h.d.Pool, workflowRunCountParams(row.ID, filters))
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: count workflow runs", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	workflowRows, err := q.ListWorkflowRunWorkflowsForRepo(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list workflows", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	basePath := "/" + owner.Username + "/" + row.Name + "/actions"
	workflows, allRunCount, activeWorkflowName := actionsWorkflowViews(workflowRows, filters, basePath)
	runViews := make([]actionsListRunView, 0, len(runs))
	now := time.Now()
	for _, run := range runs {
		runViews = append(runViews, actionsListRunViewFromRow(run, owner.Username, row.Name, now))
	}

	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = "Actions · " + row.Name
	data["Runs"] = runViews
	data["Workflows"] = workflows
	data["RunCount"] = allRunCount
	data["FilteredRunCount"] = filteredCount
	data["ActiveWorkflowName"] = activeWorkflowName
	data["Filters"] = filters
	data["EventOptions"] = actionsEventOptions(filters.Event)
	data["StatusOptions"] = actionsStatusOptions(filters.Status)
	data["ConclusionOptions"] = actionsConclusionOptions(filters.Conclusion)
	data["Pagination"] = actionsPagination(basePath, filters, filteredCount, int64(len(runViews)))
	if err := h.d.Render.RenderPage(w, r, "repo/actions", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo actions render", "error", err)
	}
}

func actionsListFiltersFromRequest(r *http.Request) actionsListFilters {
	q := r.URL.Query()
	f := actionsListFilters{
		Workflow:   trimFilter(q.Get("workflow"), 256),
		Branch:     trimFilter(q.Get("branch"), 256),
		Event:      validWorkflowRunEvent(q.Get("event")),
		Status:     validWorkflowRunStatus(q.Get("status")),
		Conclusion: validWorkflowRunConclusion(q.Get("conclusion")),
		Actor:      trimFilter(q.Get("actor"), 39),
		Page:       parseActionsPage(q.Get("page")),
	}
	f.HasAny = f.Workflow != "" || f.Branch != "" || f.Event != "" || f.Status != "" || f.Conclusion != "" || f.Actor != ""
	return f
}

func trimFilter(v string, max int) string {
	v = strings.TrimSpace(v)
	if len(v) > max {
		return v[:max]
	}
	return v
}

func parseActionsPage(v string) int32 {
	page, err := strconv.ParseInt(strings.TrimSpace(v), 10, 32)
	if err != nil || page < 1 {
		return 1
	}
	if page > 100000 {
		return 100000
	}
	return int32(page)
}

func workflowRunListParams(repoID int64, filters actionsListFilters) actionsdb.ListWorkflowRunsForRepoParams {
	return actionsdb.ListWorkflowRunsForRepoParams{
		RepoID:        repoID,
		WorkflowFile:  nullableText(filters.Workflow),
		HeadRef:       nullableText(filters.Branch),
		Event:         nullableWorkflowRunEvent(filters.Event),
		Status:        nullableWorkflowRunStatus(filters.Status),
		Conclusion:    nullableWorkflowRunConclusion(filters.Conclusion),
		ActorUsername: nullableText(filters.Actor),
	}
}

func workflowRunCountParams(repoID int64, filters actionsListFilters) actionsdb.CountWorkflowRunsForRepoParams {
	return actionsdb.CountWorkflowRunsForRepoParams{
		RepoID:        repoID,
		WorkflowFile:  nullableText(filters.Workflow),
		HeadRef:       nullableText(filters.Branch),
		Event:         nullableWorkflowRunEvent(filters.Event),
		Status:        nullableWorkflowRunStatus(filters.Status),
		Conclusion:    nullableWorkflowRunConclusion(filters.Conclusion),
		ActorUsername: nullableText(filters.Actor),
	}
}

func nullableText(v string) pgtype.Text {
	if v == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: v, Valid: true}
}

func nullableWorkflowRunEvent(v string) actionsdb.NullWorkflowRunEvent {
	if v == "" {
		return actionsdb.NullWorkflowRunEvent{}
	}
	return actionsdb.NullWorkflowRunEvent{WorkflowRunEvent: actionsdb.WorkflowRunEvent(v), Valid: true}
}

func nullableWorkflowRunStatus(v string) actionsdb.NullWorkflowRunStatus {
	if v == "" {
		return actionsdb.NullWorkflowRunStatus{}
	}
	return actionsdb.NullWorkflowRunStatus{WorkflowRunStatus: actionsdb.WorkflowRunStatus(v), Valid: true}
}

func nullableWorkflowRunConclusion(v string) actionsdb.NullCheckConclusion {
	if v == "" {
		return actionsdb.NullCheckConclusion{}
	}
	return actionsdb.NullCheckConclusion{CheckConclusion: actionsdb.CheckConclusion(v), Valid: true}
}

func actionsWorkflowViews(rows []actionsdb.ListWorkflowRunWorkflowsForRepoRow, filters actionsListFilters, basePath string) ([]actionsWorkflowView, int64, string) {
	params := actionsFilterParams(filters)
	params.Del("page")
	out := make([]actionsWorkflowView, 0, len(rows))
	var total int64
	activeName := ""
	for _, row := range rows {
		total += row.RunCount
		name := workflowDisplayName(row.WorkflowName, row.WorkflowFile)
		p := cloneValues(params)
		p.Set("workflow", row.WorkflowFile)
		active := filters.Workflow == row.WorkflowFile
		if active {
			activeName = name
		}
		out = append(out, actionsWorkflowView{
			File:   row.WorkflowFile,
			Name:   name,
			Count:  row.RunCount,
			Href:   pathWithQuery(basePath, p),
			Active: active,
		})
	}
	return out, total, activeName
}

func actionsListRunViewFromRow(row actionsdb.ListWorkflowRunsForRepoRow, owner, repoName string, now time.Time) actionsListRunView {
	stateText, stateClass, stateIcon := workflowRunState(row.Status, row.Conclusion)
	title := workflowDisplayName(row.WorkflowName, row.WorkflowFile)
	updatedAt := row.UpdatedAt.Time
	if updatedAt.IsZero() {
		updatedAt = row.CreatedAt.Time
	}
	return actionsListRunView{
		ID:            row.ID,
		RunIndex:      row.RunIndex,
		WorkflowFile:  row.WorkflowFile,
		WorkflowName:  row.WorkflowName,
		Title:         title,
		HeadSha:       row.HeadSha,
		HeadShaShort:  shortSHA(row.HeadSha),
		HeadRef:       row.HeadRef,
		Event:         string(row.Event),
		EventLabel:    workflowRunEventLabel(string(row.Event)),
		ActorUsername: row.ActorUsername,
		StateText:     stateText,
		StateClass:    stateClass,
		StateIcon:     stateIcon,
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     updatedAt,
		Duration:      workflowRunDuration(row.Status, row.StartedAt, row.CompletedAt, row.CreatedAt, updatedAt, now),
		Href:          "/" + owner + "/" + repoName + "/actions/runs/" + strconv.FormatInt(row.RunIndex, 10),
	}
}

func workflowDisplayName(name, file string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	base := path.Base(file)
	ext := path.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	if base == "." || base == "/" || base == "" {
		return file
	}
	return base
}

func workflowRunState(status actionsdb.WorkflowRunStatus, conclusion actionsdb.NullCheckConclusion) (string, string, string) {
	switch status {
	case actionsdb.WorkflowRunStatusQueued:
		return "Queued", "pending", "dot-fill"
	case actionsdb.WorkflowRunStatusRunning:
		return "In progress", "running", "dot-fill"
	case actionsdb.WorkflowRunStatusCancelled:
		return "Cancelled", "neutral", "x-circle"
	case actionsdb.WorkflowRunStatusCompleted:
		if !conclusion.Valid {
			return "Completed", "neutral", "check-circle"
		}
	default:
		if !conclusion.Valid {
			return string(status), "neutral", "dot-fill"
		}
	}
	switch conclusion.CheckConclusion {
	case actionsdb.CheckConclusionSuccess, actionsdb.CheckConclusionSkipped, actionsdb.CheckConclusionNeutral:
		return "Success", "success", "check-circle-fill"
	case actionsdb.CheckConclusionFailure, actionsdb.CheckConclusionTimedOut, actionsdb.CheckConclusionActionRequired:
		return "Failure", "failure", "x-circle-fill"
	case actionsdb.CheckConclusionCancelled, actionsdb.CheckConclusionStale:
		return "Cancelled", "neutral", "x-circle"
	default:
		return string(conclusion.CheckConclusion), "neutral", "dot-fill"
	}
}

func workflowRunDuration(status actionsdb.WorkflowRunStatus, startedAt, completedAt, createdAt pgtype.Timestamptz, updatedAt, now time.Time) string {
	if status == actionsdb.WorkflowRunStatusQueued {
		return "—"
	}
	start := createdAt.Time
	if startedAt.Valid {
		start = startedAt.Time
	}
	end := updatedAt
	if status == actionsdb.WorkflowRunStatusRunning {
		end = now
	} else if completedAt.Valid {
		end = completedAt.Time
	}
	return formatDuration(end.Sub(start))
}

func validWorkflowRunEvent(v string) string {
	switch strings.TrimSpace(v) {
	case "push", "pull_request", "schedule", "workflow_dispatch":
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func validWorkflowRunStatus(v string) string {
	switch strings.TrimSpace(v) {
	case "queued", "running", "completed", "cancelled":
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func validWorkflowRunConclusion(v string) string {
	switch strings.TrimSpace(v) {
	case "success", "failure", "neutral", "cancelled", "skipped", "timed_out", "action_required", "stale":
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

func actionsEventOptions(selected string) []actionsFilterOption {
	return selectedOptions(selected, []actionsFilterOption{
		{Value: "", Label: "Any event"},
		{Value: "push", Label: "push"},
		{Value: "pull_request", Label: "pull_request"},
		{Value: "schedule", Label: "schedule"},
		{Value: "workflow_dispatch", Label: "workflow_dispatch"},
	})
}

func actionsStatusOptions(selected string) []actionsFilterOption {
	return selectedOptions(selected, []actionsFilterOption{
		{Value: "", Label: "Any status"},
		{Value: "queued", Label: "queued"},
		{Value: "running", Label: "running"},
		{Value: "completed", Label: "completed"},
		{Value: "cancelled", Label: "cancelled"},
	})
}

func actionsConclusionOptions(selected string) []actionsFilterOption {
	return selectedOptions(selected, []actionsFilterOption{
		{Value: "", Label: "Any conclusion"},
		{Value: "success", Label: "success"},
		{Value: "failure", Label: "failure"},
		{Value: "neutral", Label: "neutral"},
		{Value: "cancelled", Label: "cancelled"},
		{Value: "skipped", Label: "skipped"},
		{Value: "timed_out", Label: "timed_out"},
		{Value: "action_required", Label: "action_required"},
		{Value: "stale", Label: "stale"},
	})
}

func selectedOptions(selected string, opts []actionsFilterOption) []actionsFilterOption {
	out := make([]actionsFilterOption, len(opts))
	copy(out, opts)
	for i := range out {
		out[i].Selected = out[i].Value == selected
	}
	return out
}

func workflowRunEventLabel(v string) string {
	switch v {
	case "pull_request":
		return "pull request"
	case "workflow_dispatch":
		return "workflow dispatch"
	default:
		return v
	}
}

func actionsPagination(basePath string, filters actionsListFilters, total, pageRows int64) actionsPaginationView {
	offset := int64((filters.Page - 1) * actionsRunsPageSize)
	view := actionsPaginationView{
		Page:     filters.Page,
		PageSize: actionsRunsPageSize,
		Total:    total,
		HasPrev:  filters.Page > 1,
		HasNext:  offset+pageRows < total,
	}
	if total == 0 {
		view.ResultText = "No workflow runs"
		return view
	}
	view.Start = offset + 1
	view.End = offset + pageRows
	view.ResultText = strconv.FormatInt(view.Start, 10) + "-" + strconv.FormatInt(view.End, 10) + " of " + strconv.FormatInt(total, 10)
	if view.HasPrev {
		p := actionsFilterParams(filters)
		if filters.Page <= 2 {
			p.Del("page")
		} else {
			p.Set("page", strconv.FormatInt(int64(filters.Page-1), 10))
		}
		view.PrevHref = pathWithQuery(basePath, p)
	}
	if view.HasNext {
		p := actionsFilterParams(filters)
		p.Set("page", strconv.FormatInt(int64(filters.Page+1), 10))
		view.NextHref = pathWithQuery(basePath, p)
	}
	return view
}

func actionsFilterParams(filters actionsListFilters) url.Values {
	v := url.Values{}
	if filters.Workflow != "" {
		v.Set("workflow", filters.Workflow)
	}
	if filters.Branch != "" {
		v.Set("branch", filters.Branch)
	}
	if filters.Event != "" {
		v.Set("event", filters.Event)
	}
	if filters.Status != "" {
		v.Set("status", filters.Status)
	}
	if filters.Conclusion != "" {
		v.Set("conclusion", filters.Conclusion)
	}
	if filters.Actor != "" {
		v.Set("actor", filters.Actor)
	}
	if filters.Page > 1 {
		v.Set("page", strconv.FormatInt(int64(filters.Page), 10))
	}
	return v
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for key, values := range v {
		for _, value := range values {
			out.Add(key, value)
		}
	}
	return out
}

func pathWithQuery(basePath string, q url.Values) string {
	if encoded := q.Encode(); encoded != "" {
		return basePath + "?" + encoded
	}
	return basePath
}

func (h *Handlers) repoActionRun(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	suiteID, err := strconv.ParseInt(chi.URLParam(r, "suiteID"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	suite, err := h.cq.GetCheckSuiteForRepo(r.Context(), h.d.Pool, checksdb.GetCheckSuiteForRepoParams{
		RepoID: row.ID,
		ID:     suiteID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: get suite", "suite_id", suiteID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	runs, err := h.cq.ListCheckRunsBySuite(r.Context(), h.d.Pool, suite.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: get suite runs", "suite_id", suiteID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	view := actionsSuiteViewFromGetRow(suite, runs)
	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = view.Title + " · " + row.Name
	data["Run"] = view
	data["CSRFToken"] = middleware.CSRFTokenForRequest(r)
	if err := h.d.Render.RenderPage(w, r, "repo/action_run", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo action run render", "suite_id", suiteID, "error", err)
	}
}

func actionsSuiteViewFromGetRow(row checksdb.GetCheckSuiteForRepoRow, runs []checksdb.CheckRun) actionsSuiteView {
	return actionsSuiteViewFromParts(
		row.ID,
		row.HeadSha,
		row.AppSlug,
		row.Status,
		row.Conclusion,
		row.CreatedAt.Time,
		row.UpdatedAt.Time,
		row.PullNumber,
		row.PullTitle,
		row.PullAuthorUsername,
		row.HeadRef,
		row.BaseRef,
		runs,
	)
}

func actionsSuiteViewFromParts(
	id int64,
	headSHA string,
	appSlug string,
	status checksdb.CheckStatus,
	conclusion checksdb.NullCheckConclusion,
	createdAt time.Time,
	updatedAt time.Time,
	pullNumber int64,
	pullTitle string,
	pullAuthorUsername string,
	headRef string,
	baseRef string,
	runs []checksdb.CheckRun,
) actionsSuiteView {
	title := pullTitle
	if title == "" {
		title = appSlug + " checks for " + shortSHA(headSHA)
	}
	stateText, stateClass, stateIcon := checkActionState(status, conclusion)
	runViews := make([]actionsRunView, 0, len(runs))
	annotationCount := 0
	for _, run := range runs {
		view := actionsRunViewFromRun(run)
		if view.SummaryHTML != "" {
			annotationCount++
		}
		runViews = append(runViews, view)
	}
	return actionsSuiteView{
		ID:                 id,
		AppSlug:            appSlug,
		Title:              title,
		HeadSha:            headSHA,
		HeadShaShort:       shortSHA(headSHA),
		PullNumber:         pullNumber,
		PullAuthorUsername: pullAuthorUsername,
		HeadRef:            headRef,
		BaseRef:            baseRef,
		RunCount:           len(runs),
		StateText:          stateText,
		StateClass:         stateClass,
		StateIcon:          stateIcon,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
		Duration:           actionSuiteDuration(runs, createdAt, updatedAt),
		Runs:               runViews,
		AnnotationCount:    annotationCount,
	}
}

func actionsRunViewFromRun(run checksdb.CheckRun) actionsRunView {
	stateText, stateClass, stateIcon := checkActionState(run.Status, run.Conclusion)
	start := run.CreatedAt.Time
	if run.StartedAt.Valid {
		start = run.StartedAt.Time
	}
	end := run.UpdatedAt.Time
	if run.CompletedAt.Valid {
		end = run.CompletedAt.Time
	}
	return actionsRunView{
		ID:          run.ID,
		Name:        run.Name,
		StateText:   stateText,
		StateClass:  stateClass,
		StateIcon:   stateIcon,
		Duration:    formatDuration(end.Sub(start)),
		CompletedAt: end,
		DetailsURL:  run.DetailsUrl,
		SummaryHTML: renderCheckSummary(run.Output),
	}
}

func checkActionState(status checksdb.CheckStatus, conclusion checksdb.NullCheckConclusion) (string, string, string) {
	if !conclusion.Valid {
		switch status {
		case checksdb.CheckStatusCompleted:
			return "Completed", "neutral", "check-circle"
		case checksdb.CheckStatusInProgress:
			return "In progress", "running", "dot-fill"
		case checksdb.CheckStatusQueued, checksdb.CheckStatusPending:
			return "Queued", "pending", "dot-fill"
		default:
			return string(status), "neutral", "dot-fill"
		}
	}
	switch conclusion.CheckConclusion {
	case checksdb.CheckConclusionSuccess, checksdb.CheckConclusionSkipped, checksdb.CheckConclusionNeutral:
		return "Success", "success", "check-circle-fill"
	case checksdb.CheckConclusionFailure, checksdb.CheckConclusionTimedOut, checksdb.CheckConclusionActionRequired:
		return "Failure", "failure", "x-circle-fill"
	case checksdb.CheckConclusionCancelled, checksdb.CheckConclusionStale:
		return "Cancelled", "neutral", "x-circle"
	default:
		return string(conclusion.CheckConclusion), "neutral", "dot-fill"
	}
}

func actionSuiteDuration(runs []checksdb.CheckRun, createdAt, updatedAt time.Time) string {
	if len(runs) == 0 {
		return formatDuration(updatedAt.Sub(createdAt))
	}
	var start, end time.Time
	for _, run := range runs {
		runStart := run.CreatedAt.Time
		if run.StartedAt.Valid {
			runStart = run.StartedAt.Time
		}
		runEnd := run.UpdatedAt.Time
		if run.CompletedAt.Valid {
			runEnd = run.CompletedAt.Time
		}
		if start.IsZero() || runStart.Before(start) {
			start = runStart
		}
		if end.IsZero() || runEnd.After(end) {
			end = runEnd
		}
	}
	return formatDuration(end.Sub(start))
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	if d < time.Minute {
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
	if d < time.Hour {
		mins := int(d / time.Minute)
		secs := int((d % time.Minute) / time.Second)
		if secs == 0 {
			return strconv.Itoa(mins) + "m"
		}
		return strconv.Itoa(mins) + "m " + strconv.Itoa(secs) + "s"
	}
	hours := int(d / time.Hour)
	mins := int((d % time.Hour) / time.Minute)
	return strconv.Itoa(hours) + "h " + strconv.Itoa(mins) + "m"
}

func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
