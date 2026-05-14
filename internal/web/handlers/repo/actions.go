// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
)

const (
	actionsRunsPageSize       = int32(20)
	actionsStepLogRenderLimit = 1 << 20
)

type actionsWorkflowView struct {
	File   string
	Name   string
	Count  int64
	Href   string
	Active bool
}

type actionsSidebarView struct {
	AllHref     string
	AllRunCount int64
	AllActive   bool
	Workflows   []actionsWorkflowView
	Management  []actionsManagementNavItem
}

type actionsManagementNavItem struct {
	Key    string
	Label  string
	Icon   string
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

type actionsRunDetailView struct {
	ID               int64
	RunIndex         int64
	WorkflowFile     string
	WorkflowName     string
	Title            string
	HeadSha          string
	HeadShaShort     string
	HeadRef          string
	Event            string
	EventLabel       string
	ActorUsername    string
	StateText        string
	StateClass       string
	StateIcon        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Duration         string
	IsTerminal       bool
	NeedApproval     bool
	ApprovalPending  bool
	ApprovalRejected bool
	ApprovalReason   string
	ApproveHref      string
	RejectHref       string
	CanApprove       bool
	StatusHref       string
	CancelHref       string
	CanCancel        bool
	RerunHref        string
	CanRerun         bool
	ParentRunIndex   int64
	ParentRunHref    string
	ActionsHref      string
	CodeHref         string
	ArtifactCount    int
	JobCount         int
	CompletedCount   int
	FailureCount     int
	Jobs             []actionsJobDetailView
	Stages           []actionsJobStageView
}

type actionsJobDetailView struct {
	ID         int64
	JobIndex   int32
	JobKey     string
	Name       string
	RunsOn     string
	Needs      []string
	NeedsText  string
	WaitReason string
	StateText  string
	StateClass string
	StateIcon  string
	Duration   string
	Anchor     string
	CancelHref string
	CanCancel  bool
	// CancelRequested is true after a running job has been asked to stop but
	// before its runner has reported the terminal cancelled state.
	CancelRequested bool
	IsCancellable   bool
	Depth           int
	Steps           []actionsStepDetailView
}

type actionsStepDetailView struct {
	ID           int64
	StepIndex    int32
	StepID       string
	Name         string
	Kind         string
	Detail       string
	StateText    string
	StateClass   string
	StateIcon    string
	Duration     string
	IsTerminal   bool
	LogByteCount int64
	LogHref      string
}

type actionsJobStageView struct {
	Index int
	Jobs  []actionsJobDetailView
}

type actionsStepLogView struct {
	Run          actionsRunDetailView
	Job          actionsJobDetailView
	Step         actionsStepDetailView
	LogText      string
	LogSource    string
	LogError     string
	LogTruncated bool
	StreamHref   string
	DownloadURL  string
	BackHref     string
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
	sidebar := actionsSidebar(basePath, workflows, allRunCount, filters.Workflow == "", "")
	runViews := make([]actionsListRunView, 0, len(runs))
	now := time.Now()
	for _, run := range runs {
		runViews = append(runViews, actionsListRunViewFromRow(run, owner.Username, row.Name, now))
	}
	dispatchWorkflows, err := h.actionsDispatchWorkflowViews(r.Context(), row, owner.Username)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: discover dispatch workflows", "repo_id", row.ID, "error", err)
	}

	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = "Actions · " + row.Name
	data["Runs"] = runViews
	data["Workflows"] = workflows
	data["ActionsSidebar"] = sidebar
	data["DispatchWorkflows"] = dispatchWorkflows
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

func actionsSidebar(basePath string, workflows []actionsWorkflowView, allRunCount int64, allActive bool, activeManagement string) actionsSidebarView {
	if activeManagement != "" {
		allActive = false
		workflows = inactiveActionsWorkflows(workflows)
	}
	return actionsSidebarView{
		AllHref:     basePath,
		AllRunCount: allRunCount,
		AllActive:   allActive,
		Workflows:   workflows,
		Management:  actionsManagementNavItems(basePath, activeManagement),
	}
}

func inactiveActionsWorkflows(workflows []actionsWorkflowView) []actionsWorkflowView {
	out := make([]actionsWorkflowView, len(workflows))
	copy(out, workflows)
	for i := range out {
		out[i].Active = false
	}
	return out
}

func actionsManagementNavItems(basePath, active string) []actionsManagementNavItem {
	items := []actionsManagementNavItem{
		{Key: "caches", Label: "Caches", Icon: "cache", Href: basePath + "/caches"},
		{Key: "attestations", Label: "Attestations", Icon: "shield-check", Href: basePath + "/attestations"},
		{Key: "runners", Label: "Runners", Icon: "server", Href: basePath + "/runners"},
		{Key: "usage", Label: "Usage metrics", Icon: "pulse", Href: basePath + "/metrics/usage"},
		{Key: "performance", Label: "Performance metrics", Icon: "stopwatch", Href: basePath + "/metrics/performance"},
	}
	for i := range items {
		items[i].Active = items[i].Key == active
	}
	return items
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
	runIndex, ok := parsePositiveInt64Param(r, "runIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	view, err := h.loadActionsRunDetail(r.Context(), row.ID, owner.Username, row.Name, runIndex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: get run detail", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	h.applyActionsLifecycleControls(r, row, &view)

	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = view.Title + " #" + strconv.FormatInt(view.RunIndex, 10) + " · " + row.Name
	data["Run"] = view
	data["UseHTMX"] = true
	if err := h.d.Render.RenderPage(w, r, "repo/action_run", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo action run render", "run_index", runIndex, "error", err)
	}
}

func (h *Handlers) repoActionRunStatus(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	runIndex, ok := parsePositiveInt64Param(r, "runIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	view, err := h.loadActionsRunDetail(r.Context(), row.ID, owner.Username, row.Name, runIndex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: get run status", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}

	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Run"] = view
	if err := h.d.Render.RenderFragment(w, "repo/action_run_status", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo action run status render", "run_index", runIndex, "error", err)
	}
}

func (h *Handlers) repoActionStepLog(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	runIndex, ok := parsePositiveInt64Param(r, "runIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	jobIndex, ok := parseNonNegativeInt32Param(r, "jobIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	stepIndex, ok := parseNonNegativeInt32Param(r, "stepIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	run, err := h.loadActionsRunDetail(r.Context(), row.ID, owner.Username, row.Name, runIndex)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: get run for step log", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}

	job, step, ok := findActionStep(run, jobIndex, stepIndex)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	logContent, err := h.loadStepLogContent(r.Context(), step.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: load step log", "step_id", step.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	view := actionsStepLogView{
		Run:          run,
		Job:          job,
		Step:         step,
		LogText:      logContent.Text,
		LogSource:    logContent.Source,
		LogError:     logContent.Error,
		LogTruncated: logContent.Truncated,
		DownloadURL:  logContent.DownloadURL,
		BackHref:     run.ActionsHref + "/runs/" + strconv.FormatInt(run.RunIndex, 10) + "#job-" + strconv.FormatInt(int64(job.JobIndex), 10),
	}
	if !step.IsTerminal && logContent.Error == "" && logContent.DownloadURL == "" {
		view.StreamHref = step.LogHref + "/log/stream?after=" + strconv.FormatInt(int64(logContent.LastSeq), 10)
	}
	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = step.Name + " · " + run.Title + " #" + strconv.FormatInt(run.RunIndex, 10)
	data["Log"] = view
	if err := h.d.Render.RenderPage(w, r, "repo/action_step_log", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo action step log render", "run_index", runIndex, "job_index", jobIndex, "step_index", stepIndex, "error", err)
	}
}

func (h *Handlers) loadActionsRunDetail(ctx context.Context, repoID int64, owner, repoName string, runIndex int64) (actionsRunDetailView, error) {
	q := actionsdb.New()
	run, err := q.GetWorkflowRunForRepoByIndex(ctx, h.d.Pool, actionsdb.GetWorkflowRunForRepoByIndexParams{
		RepoID:   repoID,
		RunIndex: runIndex,
	})
	if err != nil {
		return actionsRunDetailView{}, err
	}
	jobs, err := q.ListJobsForRun(ctx, h.d.Pool, run.ID)
	if err != nil {
		return actionsRunDetailView{}, err
	}
	artifacts, err := q.ListArtifactsForRun(ctx, h.d.Pool, run.ID)
	if err != nil {
		return actionsRunDetailView{}, err
	}

	basePath := "/" + owner + "/" + repoName + "/actions"
	runPath := basePath + "/runs/" + strconv.FormatInt(run.RunIndex, 10)
	now := time.Now()
	stateText, stateClass, stateIcon := workflowRunState(run.Status, run.Conclusion)
	approvalPending := run.NeedApproval && !run.ApprovedByUserID.Valid && run.Status == actionsdb.WorkflowRunStatusQueued
	if approvalPending {
		stateText, stateClass, stateIcon = "Approval required", "pending", "clock"
	}
	updatedAt := pgTime(run.UpdatedAt, run.CreatedAt.Time)
	view := actionsRunDetailView{
		ID:              run.ID,
		RunIndex:        run.RunIndex,
		WorkflowFile:    run.WorkflowFile,
		WorkflowName:    run.WorkflowName,
		Title:           workflowDisplayName(run.WorkflowName, run.WorkflowFile),
		HeadSha:         run.HeadSha,
		HeadShaShort:    shortSHA(run.HeadSha),
		HeadRef:         run.HeadRef,
		Event:           string(run.Event),
		EventLabel:      workflowRunEventLabel(string(run.Event)),
		ActorUsername:   run.ActorUsername,
		StateText:       stateText,
		StateClass:      stateClass,
		StateIcon:       stateIcon,
		CreatedAt:       run.CreatedAt.Time,
		UpdatedAt:       updatedAt,
		Duration:        workflowRunDuration(run.Status, run.StartedAt, run.CompletedAt, run.CreatedAt, updatedAt, now),
		IsTerminal:      workflowRunTerminal(run.Status),
		NeedApproval:    run.NeedApproval,
		ApprovalPending: approvalPending,
		StatusHref:      runPath + "/status",
		ApproveHref:     runPath + "/approve",
		RejectHref:      runPath + "/reject",
		CancelHref:      runPath + "/cancel",
		RerunHref:       runPath + "/rerun",
		ActionsHref:     basePath,
		CodeHref:        "/" + owner + "/" + repoName + "/tree/" + codeTarget(run.HeadRef, run.HeadSha),
		ArtifactCount:   len(artifacts),
		JobCount:        len(jobs),
		CompletedCount:  0,
		FailureCount:    0,
		Jobs:            make([]actionsJobDetailView, 0, len(jobs)),
	}
	if run.ParentRunID.Valid {
		parent, err := q.GetWorkflowRunByID(ctx, h.d.Pool, run.ParentRunID.Int64)
		if err == nil && parent.RepoID == repoID {
			view.ParentRunIndex = parent.RunIndex
			view.ParentRunHref = basePath + "/runs/" + strconv.FormatInt(parent.RunIndex, 10)
		}
	}
	if run.NeedApproval {
		if approval, err := q.GetWorkflowRunApproval(ctx, h.d.Pool, run.ID); err == nil {
			view.ApprovalReason = approval.RequestedReason
			view.ApprovalRejected = approval.RejectedAt.Valid
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return actionsRunDetailView{}, err
		}
	}
	for _, job := range jobs {
		steps, err := q.ListStepsForJob(ctx, h.d.Pool, job.ID)
		if err != nil {
			return actionsRunDetailView{}, err
		}
		jobView := actionsJobDetailViewFromRow(job, owner, repoName, run.RunIndex, now)
		if approvalPending && job.Status == actionsdb.WorkflowJobStatusQueued {
			jobView.WaitReason = "Waiting for maintainer approval"
		}
		jobView.Steps = make([]actionsStepDetailView, 0, len(steps))
		for _, step := range steps {
			jobView.Steps = append(jobView.Steps, actionsStepDetailViewFromRow(step, owner, repoName, run.RunIndex, job.JobIndex, now))
		}
		if job.Status == actionsdb.WorkflowJobStatusCompleted || job.Status == actionsdb.WorkflowJobStatusCancelled || job.Status == actionsdb.WorkflowJobStatusSkipped {
			view.CompletedCount++
		}
		if jobView.StateClass == "failure" {
			view.FailureCount++
		}
		view.Jobs = append(view.Jobs, jobView)
	}
	view.Stages = actionsJobStages(view.Jobs)
	return view, nil
}

func actionsJobDetailViewFromRow(row actionsdb.ListJobsForRunRow, owner, repoName string, runIndex int64, now time.Time) actionsJobDetailView {
	stateText, stateClass, stateIcon := workflowJobState(row.Status, row.Conclusion)
	name := strings.TrimSpace(row.JobName)
	if name == "" {
		name = row.JobKey
	}
	return actionsJobDetailView{
		ID:         row.ID,
		JobIndex:   row.JobIndex,
		JobKey:     row.JobKey,
		Name:       name,
		RunsOn:     row.RunsOn,
		Needs:      append([]string(nil), row.NeedsJobs...),
		NeedsText:  strings.Join(row.NeedsJobs, ", "),
		WaitReason: queuedJobWaitReason(row),
		StateText:  stateText,
		StateClass: stateClass,
		StateIcon:  stateIcon,
		Duration:   actionItemDuration(string(row.Status), string(actionsdb.WorkflowJobStatusQueued), row.StartedAt, row.CompletedAt, row.CreatedAt, row.UpdatedAt, now),
		Anchor:     "job-" + strconv.FormatInt(int64(row.JobIndex), 10),
		CancelHref: "/" + owner + "/" + repoName + "/actions/runs/" + strconv.FormatInt(runIndex, 10) +
			"/jobs/" + strconv.FormatInt(int64(row.JobIndex), 10) + "/cancel",
		CancelRequested: row.CancelRequested,
		IsCancellable: row.Status == actionsdb.WorkflowJobStatusQueued ||
			row.Status == actionsdb.WorkflowJobStatusRunning,
	}
}

func queuedJobWaitReason(row actionsdb.ListJobsForRunRow) string {
	if row.Status != actionsdb.WorkflowJobStatusQueued {
		return ""
	}
	runsOn := strings.TrimSpace(row.RunsOn)
	hasNeeds := len(row.NeedsJobs) > 0
	switch {
	case runsOn != "" && hasNeeds:
		return "Waiting for dependencies or runner with labels: " + runsOn
	case runsOn != "":
		return "Waiting for runner with labels: " + runsOn
	case hasNeeds:
		return "Waiting for dependencies: " + strings.Join(row.NeedsJobs, ", ")
	default:
		return "Waiting for runner"
	}
}

func actionsStepDetailViewFromRow(row actionsdb.ListStepsForJobRow, owner, repoName string, runIndex int64, jobIndex int32, now time.Time) actionsStepDetailView {
	stateText, stateClass, stateIcon := workflowStepState(row.Status, row.Conclusion)
	name, kind, detail := workflowStepDisplay(row)
	return actionsStepDetailView{
		ID:           row.ID,
		StepIndex:    row.StepIndex,
		StepID:       row.StepID,
		Name:         name,
		Kind:         kind,
		Detail:       detail,
		StateText:    stateText,
		StateClass:   stateClass,
		StateIcon:    stateIcon,
		Duration:     actionItemDuration(string(row.Status), string(actionsdb.WorkflowStepStatusQueued), row.StartedAt, row.CompletedAt, row.CreatedAt, row.UpdatedAt, now),
		IsTerminal:   workflowStepTerminal(row.Status),
		LogByteCount: row.LogByteCount,
		LogHref: "/" + owner + "/" + repoName + "/actions/runs/" + strconv.FormatInt(runIndex, 10) +
			"/jobs/" + strconv.FormatInt(int64(jobIndex), 10) +
			"/steps/" + strconv.FormatInt(int64(row.StepIndex), 10),
	}
}

func workflowStepDisplay(row actionsdb.ListStepsForJobRow) (name, kind, detail string) {
	if row.UsesAlias != "" {
		kind = "uses"
		detail = row.UsesAlias
	} else {
		kind = "run"
		detail = firstCommandLine(row.RunCommand)
	}
	name = strings.TrimSpace(row.StepName)
	if name == "" {
		name = strings.TrimSpace(detail)
	}
	if name == "" {
		name = "Step " + strconv.Itoa(int(row.StepIndex)+1)
	}
	if len(detail) > 120 {
		detail = detail[:117] + "..."
	}
	return name, kind, detail
}

func firstCommandLine(command string) string {
	for _, line := range strings.Split(command, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func actionsJobStages(jobs []actionsJobDetailView) []actionsJobStageView {
	indexByKey := make(map[string]int, len(jobs))
	for i := range jobs {
		indexByKey[jobs[i].JobKey] = i
	}
	state := make(map[string]int, len(jobs))
	var depthFor func(string) int
	depthFor = func(key string) int {
		i, ok := indexByKey[key]
		if !ok {
			return 0
		}
		switch state[key] {
		case 1:
			return 0
		case 2:
			return jobs[i].Depth
		}
		state[key] = 1
		depth := 0
		for _, need := range jobs[i].Needs {
			if _, ok := indexByKey[need]; ok {
				if depDepth := depthFor(need) + 1; depDepth > depth {
					depth = depDepth
				}
			}
		}
		jobs[i].Depth = depth
		state[key] = 2
		return depth
	}
	maxDepth := 0
	for i := range jobs {
		depth := depthFor(jobs[i].JobKey)
		if depth > maxDepth {
			maxDepth = depth
		}
	}
	stages := make([]actionsJobStageView, maxDepth+1)
	for i := range stages {
		stages[i].Index = i
	}
	for _, job := range jobs {
		stages[job.Depth].Jobs = append(stages[job.Depth].Jobs, job)
	}
	return stages
}

func workflowJobState(status actionsdb.WorkflowJobStatus, conclusion actionsdb.NullCheckConclusion) (string, string, string) {
	if status == actionsdb.WorkflowJobStatusSkipped && !conclusion.Valid {
		return "Skipped", "neutral", "dash"
	}
	if status == actionsdb.WorkflowJobStatusCompleted && conclusion.Valid {
		return workflowConclusionState(conclusion.CheckConclusion)
	}
	switch status {
	case actionsdb.WorkflowJobStatusQueued:
		return "Queued", "pending", "dot-fill"
	case actionsdb.WorkflowJobStatusRunning:
		return "In progress", "running", "dot-fill"
	case actionsdb.WorkflowJobStatusCancelled:
		return "Cancelled", "neutral", "x-circle"
	case actionsdb.WorkflowJobStatusCompleted:
		return "Completed", "neutral", "check-circle"
	default:
		if conclusion.Valid {
			return workflowConclusionState(conclusion.CheckConclusion)
		}
		return string(status), "neutral", "dot-fill"
	}
}

func workflowStepState(status actionsdb.WorkflowStepStatus, conclusion actionsdb.NullCheckConclusion) (string, string, string) {
	if status == actionsdb.WorkflowStepStatusSkipped && !conclusion.Valid {
		return "Skipped", "neutral", "dash"
	}
	if status == actionsdb.WorkflowStepStatusCompleted && conclusion.Valid {
		return workflowConclusionState(conclusion.CheckConclusion)
	}
	switch status {
	case actionsdb.WorkflowStepStatusQueued:
		return "Queued", "pending", "dot-fill"
	case actionsdb.WorkflowStepStatusRunning:
		return "In progress", "running", "dot-fill"
	case actionsdb.WorkflowStepStatusCancelled:
		return "Cancelled", "neutral", "x-circle"
	case actionsdb.WorkflowStepStatusCompleted:
		return "Completed", "neutral", "check-circle"
	default:
		if conclusion.Valid {
			return workflowConclusionState(conclusion.CheckConclusion)
		}
		return string(status), "neutral", "dot-fill"
	}
}

func workflowConclusionState(conclusion actionsdb.CheckConclusion) (string, string, string) {
	switch conclusion {
	case actionsdb.CheckConclusionSuccess, actionsdb.CheckConclusionSkipped, actionsdb.CheckConclusionNeutral:
		return "Success", "success", "check-circle-fill"
	case actionsdb.CheckConclusionFailure, actionsdb.CheckConclusionTimedOut, actionsdb.CheckConclusionActionRequired:
		return "Failure", "failure", "x-circle-fill"
	case actionsdb.CheckConclusionCancelled, actionsdb.CheckConclusionStale:
		return "Cancelled", "neutral", "x-circle"
	default:
		return string(conclusion), "neutral", "dot-fill"
	}
}

func workflowRunTerminal(status actionsdb.WorkflowRunStatus) bool {
	return status == actionsdb.WorkflowRunStatusCompleted || status == actionsdb.WorkflowRunStatusCancelled
}

func workflowStepTerminal(status actionsdb.WorkflowStepStatus) bool {
	return status == actionsdb.WorkflowStepStatusCompleted ||
		status == actionsdb.WorkflowStepStatusCancelled ||
		status == actionsdb.WorkflowStepStatusSkipped
}

func actionItemDuration(status string, queuedStatus string, startedAt, completedAt, createdAt, updatedAt pgtype.Timestamptz, now time.Time) string {
	if status == queuedStatus {
		return "—"
	}
	start := createdAt.Time
	if startedAt.Valid {
		start = startedAt.Time
	}
	end := pgTime(updatedAt, now)
	if status == "running" {
		end = now
	} else if completedAt.Valid {
		end = completedAt.Time
	}
	return formatDuration(end.Sub(start))
}

func pgTime(ts pgtype.Timestamptz, fallback time.Time) time.Time {
	if ts.Valid && !ts.Time.IsZero() {
		return ts.Time
	}
	return fallback
}

func codeTarget(ref, sha string) string {
	if ref != "" {
		return ref
	}
	return sha
}

func findActionStep(run actionsRunDetailView, jobIndex, stepIndex int32) (actionsJobDetailView, actionsStepDetailView, bool) {
	for _, job := range run.Jobs {
		if job.JobIndex != jobIndex {
			continue
		}
		for _, step := range job.Steps {
			if step.StepIndex == stepIndex {
				return job, step, true
			}
		}
		return actionsJobDetailView{}, actionsStepDetailView{}, false
	}
	return actionsJobDetailView{}, actionsStepDetailView{}, false
}

type actionsStepLogContent struct {
	Text        string
	Source      string
	Error       string
	Truncated   bool
	LastSeq     int32
	DownloadURL string
}

func (h *Handlers) loadStepLogContent(ctx context.Context, stepID int64) (actionsStepLogContent, error) {
	q := actionsdb.New()
	step, err := q.GetWorkflowStepByID(ctx, h.d.Pool, stepID)
	if err != nil {
		return actionsStepLogContent{}, err
	}
	if step.LogObjectKey.Valid && step.LogObjectKey.String != "" {
		return h.loadArchivedStepLog(ctx, step.LogObjectKey.String)
	}
	chunks, err := q.ListAllStepLogChunksForStep(ctx, h.d.Pool, step.ID)
	if err != nil {
		return actionsStepLogContent{}, err
	}
	buf := bytes.NewBuffer(make([]byte, 0, minInt(actionsStepLogRenderLimit, int(step.LogByteCount)+1)))
	truncated := false
	lastSeq := int32(-1)
	for _, chunk := range chunks {
		lastSeq = chunk.Seq
		if buf.Len() >= actionsStepLogRenderLimit {
			truncated = true
			break
		}
		remaining := actionsStepLogRenderLimit - buf.Len()
		if len(chunk.Chunk) > remaining {
			_, _ = buf.Write(chunk.Chunk[:remaining])
			truncated = true
			break
		}
		_, _ = buf.Write(chunk.Chunk)
	}
	return actionsStepLogContent{
		Text:      strings.ToValidUTF8(buf.String(), "\uFFFD"),
		Source:    "SQL chunks",
		Truncated: truncated,
		LastSeq:   lastSeq,
	}, nil
}

func (h *Handlers) loadArchivedStepLog(ctx context.Context, key string) (actionsStepLogContent, error) {
	if h.d.ObjectStore == nil {
		return actionsStepLogContent{
			Source: "object storage",
			Error:  "Archived log storage is not configured for this server.",
		}, nil
	}
	rc, _, err := h.d.ObjectStore.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return actionsStepLogContent{
				Source: "object storage",
				Error:  "Archived log object was not found.",
			}, nil
		}
		return actionsStepLogContent{}, err
	}
	defer rc.Close()
	body, truncated, err := readLimitedLog(rc, actionsStepLogRenderLimit)
	if err != nil {
		return actionsStepLogContent{}, err
	}
	downloadURL, _ := h.d.ObjectStore.SignedURL(ctx, key, 15*time.Minute, http.MethodGet)
	return actionsStepLogContent{
		Text:        strings.ToValidUTF8(string(body), "\uFFFD"),
		Source:      "object storage",
		Truncated:   truncated,
		DownloadURL: downloadURL,
	}, nil
}

func readLimitedLog(r io.Reader, limit int) ([]byte, bool, error) {
	body, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(body) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}

func parsePositiveInt64Param(r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return v, err == nil && v > 0
}

func parseNonNegativeInt32Param(r *http.Request, name string) (int32, bool) {
	v, err := strconv.ParseInt(chi.URLParam(r, name), 10, 32)
	return int32(v), err == nil && v >= 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
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
