// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
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

	"github.com/tenseleyFlow/shithub/internal/actions/dispatch"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	actionsRunsPageSize       = int32(20)
	actionsStepLogRenderLimit = 1 << 20
)

var errActionsLogStorageUnavailable = errors.New("actions archived log storage unavailable")

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
	Href             string
	WorkflowHref     string
	WorkflowFileHref string
	CancelHref       string
	RerunHref        string
	CanCancel        bool
	CanRerun         bool
}

type actionsListFilters struct {
	Workflow      string
	Branch        string
	Event         string
	Status        string
	Conclusion    string
	Actor         string
	Page          int32
	HasAny        bool
	HasRunFilters bool
}

type actionsFilterOption struct {
	Value    string
	Label    string
	Selected bool
	Href     string
	Disabled bool
}

type actionsFilterMenuView struct {
	Key     string
	Label   string
	Options []actionsFilterOption
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
	AnnotationCount  int
	WarningCount     int
	ErrorCount       int
	Jobs             []actionsJobDetailView
	Stages           []actionsJobStageView
	Graph            actionsRunGraphView
	AnnotationGroups []actionsAnnotationGroupView
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

type actionsRunGraphView struct {
	CanvasWidth  int                       `json:"canvasWidth"`
	CanvasHeight int                       `json:"canvasHeight"`
	NodeWidth    int                       `json:"nodeWidth"`
	NodeHeight   int                       `json:"nodeHeight"`
	Nodes        []actionsRunGraphNodeView `json:"nodes"`
	Edges        []actionsRunGraphEdgeView `json:"edges"`
}

type actionsRunGraphNodeView struct {
	ID                 string                    `json:"id"`
	JobIndex           int32                     `json:"jobIndex"`
	JobKey             string                    `json:"jobKey"`
	Name               string                    `json:"name"`
	RunsOn             string                    `json:"runsOn,omitempty"`
	Needs              []string                  `json:"needs,omitempty"`
	NeedsText          string                    `json:"needsText,omitempty"`
	StateText          string                    `json:"stateText"`
	StateClass         string                    `json:"stateClass"`
	StateIcon          string                    `json:"stateIcon"`
	Duration           string                    `json:"duration"`
	Anchor             string                    `json:"anchor"`
	X                  int                       `json:"x"`
	Y                  int                       `json:"y"`
	Width              int                       `json:"width"`
	Height             int                       `json:"height"`
	StepCount          int                       `json:"stepCount"`
	CompletedStepCount int                       `json:"completedStepCount"`
	FailureCount       int                       `json:"failureCount"`
	Steps              []actionsRunGraphStepView `json:"steps,omitempty"`
}

type actionsRunGraphStepView struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Detail     string `json:"detail,omitempty"`
	StateText  string `json:"stateText"`
	StateClass string `json:"stateClass"`
	Duration   string `json:"duration"`
	LogHref    string `json:"logHref"`
}

type actionsRunGraphEdgeView struct {
	From string `json:"from"`
	To   string `json:"to"`
	Path string `json:"path"`
}

type actionsAnnotationGroupView struct {
	JobID       int64
	JobName     string
	JobAnchor   string
	Count       int
	Annotations []actionsAnnotationView
}

type actionsAnnotationView struct {
	ID         int64
	Level      string
	LevelLabel string
	StateClass string
	Icon       string
	Title      string
	Message    string
	Path       string
	Location   string
	StepName   string
	StepHref   string
	SourceHref string
}

type actionsStepLogView struct {
	Run          actionsRunDetailView
	Job          actionsJobDetailView
	Step         actionsStepDetailView
	Annotations  []actionsAnnotationView
	WarningCount int
	ErrorCount   int
	LogText      string
	LogSource    string
	LogError     string
	LogTruncated bool
	StreamHref   string
	DownloadHref string
	BackHref     string
}

func (h *Handlers) repoTabActions(w http.ResponseWriter, r *http.Request) {
	h.repoActionsList(w, r, "")
}

func (h *Handlers) repoActionsWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowFile, ok := actionsWorkflowFileFromRoute(r)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	h.repoActionsList(w, r, workflowFile)
}

func (h *Handlers) repoActionsList(w http.ResponseWriter, r *http.Request, routeWorkflowFile string) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}

	filters := actionsListFiltersFromRequest(r)
	if routeWorkflowFile != "" {
		filters.Workflow = routeWorkflowFile
	}
	q := actionsdb.New()
	workflowRows, err := q.ListWorkflowRunWorkflowsForRepo(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list workflows", "repo_id", row.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	filters = actionsResolveWorkflowFilter(filters, workflowRows)

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

	basePath := "/" + owner.Username + "/" + row.Name + "/actions"
	listBasePath := basePath
	includeWorkflowFilter := true
	if filters.Workflow != "" {
		if href := actionsWorkflowRoutePath(basePath, filters.Workflow); href != "" {
			listBasePath = href
			includeWorkflowFilter = false
		}
	}
	workflows, allRunCount, activeWorkflowName := actionsWorkflowViews(workflowRows, filters, basePath)
	if activeWorkflowName == "" && filters.Workflow != "" {
		activeWorkflowName = workflowDisplayName("", filters.Workflow)
	}
	sidebar := actionsSidebar(basePath, workflows, allRunCount, filters.Workflow == "", "")
	viewer := middleware.CurrentUserFromContext(r.Context())
	canManage := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(row)).Allow
	runViews := make([]actionsListRunView, 0, len(runs))
	now := time.Now()
	for _, run := range runs {
		runViews = append(runViews, actionsListRunViewFromRow(run, owner.Username, row.Name, now, canManage))
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
	data["FilterQuery"] = actionsFilterQuery(filters, includeWorkflowFilter)
	data["FilterMenus"] = actionsFilterMenus(listBasePath, filters, workflows, includeWorkflowFilter)
	data["ListBasePath"] = listBasePath
	data["RunCountLabel"] = pluralizeInt64(filteredCount, "workflow run", "workflow runs")
	data["ClearFiltersHref"] = listBasePath
	data["HasClearableFilters"] = actionsHasClearableFilters(filters, includeWorkflowFilter)
	data["ActiveWorkflowFileHref"] = actionsWorkflowFileHref(owner.Username, row.Name, codeTarget(row.DefaultBranch, ""), filters.Workflow)
	data["EventOptions"] = actionsEventOptions(filters.Event)
	data["StatusOptions"] = actionsStatusOptions(filters.Status)
	data["ConclusionOptions"] = actionsConclusionOptions(filters.Conclusion)
	data["Pagination"] = actionsPagination(listBasePath, filters, filteredCount, int64(len(runViews)), includeWorkflowFilter)
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
	if query := trimFilter(q.Get("query"), 1024); query != "" {
		parsed := parseActionsFilterQuery(query)
		if parsed.Workflow != "" {
			f.Workflow = parsed.Workflow
		}
		if parsed.Branch != "" {
			f.Branch = parsed.Branch
		}
		if parsed.Event != "" {
			f.Event = validWorkflowRunEvent(parsed.Event)
		}
		if parsed.Status != "" {
			f.Status = validWorkflowRunStatus(parsed.Status)
		}
		if parsed.Conclusion != "" {
			f.Conclusion = validWorkflowRunConclusion(parsed.Conclusion)
		}
		if parsed.Actor != "" {
			f.Actor = trimFilter(parsed.Actor, 39)
		}
	}
	f.HasRunFilters = f.Branch != "" || f.Event != "" || f.Status != "" || f.Conclusion != "" || f.Actor != ""
	f.HasAny = f.Workflow != "" || f.HasRunFilters
	return f
}

type parsedActionsFilterQuery struct {
	Workflow   string
	Branch     string
	Event      string
	Status     string
	Conclusion string
	Actor      string
}

func parseActionsFilterQuery(query string) parsedActionsFilterQuery {
	var out parsedActionsFilterQuery
	for _, token := range actionsFilterTokens(query) {
		key, value, ok := strings.Cut(token, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch key {
		case "workflow":
			out.Workflow = value
		case "branch":
			out.Branch = value
		case "event":
			out.Event = value
		case "status":
			out.Status = value
		case "conclusion":
			out.Conclusion = value
		case "actor":
			out.Actor = value
		case "is":
			if status := validWorkflowRunStatus(value); status != "" {
				out.Status = status
			} else if conclusion := validWorkflowRunConclusion(value); conclusion != "" {
				out.Conclusion = conclusion
			}
		}
	}
	return out
}

func actionsFilterTokens(query string) []string {
	var tokens []string
	var b strings.Builder
	inQuote := false
	escaped := false
	flush := func() {
		token := strings.TrimSpace(b.String())
		if token != "" {
			tokens = append(tokens, token)
		}
		b.Reset()
	}
	for _, r := range query {
		switch {
		case escaped:
			b.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case !inQuote && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return tokens
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

func actionsResolveWorkflowFilter(filters actionsListFilters, rows []actionsdb.ListWorkflowRunWorkflowsForRepoRow) actionsListFilters {
	if filters.Workflow == "" {
		return filters
	}
	if normalized, ok := normalizeActionsWorkflowFile(filters.Workflow); ok {
		filters.Workflow = normalized
		return filters
	}
	for _, row := range rows {
		name := workflowDisplayName(row.WorkflowName, row.WorkflowFile)
		base := path.Base(row.WorkflowFile)
		trimmedBase := strings.TrimSuffix(base, path.Ext(base))
		if strings.EqualFold(filters.Workflow, row.WorkflowFile) ||
			strings.EqualFold(filters.Workflow, strings.TrimPrefix(row.WorkflowFile, dispatch.WorkflowFilesDir)) ||
			strings.EqualFold(filters.Workflow, base) ||
			strings.EqualFold(filters.Workflow, trimmedBase) ||
			strings.EqualFold(filters.Workflow, name) {
			filters.Workflow = row.WorkflowFile
			return filters
		}
	}
	return filters
}

func actionsWorkflowViews(rows []actionsdb.ListWorkflowRunWorkflowsForRepoRow, filters actionsListFilters, basePath string) ([]actionsWorkflowView, int64, string) {
	params := actionsFilterParamsFor(filters, false)
	params.Del("page")
	out := make([]actionsWorkflowView, 0, len(rows))
	var total int64
	activeName := ""
	for _, row := range rows {
		total += row.RunCount
		name := workflowDisplayName(row.WorkflowName, row.WorkflowFile)
		active := filters.Workflow == row.WorkflowFile
		if active {
			activeName = name
		}
		href := actionsWorkflowRoutePath(basePath, row.WorkflowFile)
		if href == "" {
			p := cloneValues(params)
			p.Set("workflow", row.WorkflowFile)
			href = pathWithQuery(basePath, p)
		} else {
			href = pathWithQuery(href, params)
		}
		out = append(out, actionsWorkflowView{
			File:   row.WorkflowFile,
			Name:   name,
			Count:  row.RunCount,
			Href:   href,
			Active: active,
		})
	}
	return out, total, activeName
}

func actionsListRunViewFromRow(row actionsdb.ListWorkflowRunsForRepoRow, owner, repoName string, now time.Time, canManage bool) actionsListRunView {
	stateText, stateClass, stateIcon := workflowRunState(row.Status, row.Conclusion)
	title := workflowDisplayName(row.WorkflowName, row.WorkflowFile)
	updatedAt := row.UpdatedAt.Time
	if updatedAt.IsZero() {
		updatedAt = row.CreatedAt.Time
	}
	basePath := "/" + owner + "/" + repoName + "/actions"
	runHref := repoActionRunHref(owner, repoName, row.RunIndex)
	return actionsListRunView{
		ID:               row.ID,
		RunIndex:         row.RunIndex,
		WorkflowFile:     row.WorkflowFile,
		WorkflowName:     row.WorkflowName,
		Title:            title,
		HeadSha:          row.HeadSha,
		HeadShaShort:     shortSHA(row.HeadSha),
		HeadRef:          row.HeadRef,
		Event:            string(row.Event),
		EventLabel:       workflowRunEventLabel(string(row.Event)),
		ActorUsername:    row.ActorUsername,
		StateText:        stateText,
		StateClass:       stateClass,
		StateIcon:        stateIcon,
		CreatedAt:        row.CreatedAt.Time,
		UpdatedAt:        updatedAt,
		Duration:         workflowRunDuration(row.Status, row.StartedAt, row.CompletedAt, row.CreatedAt, updatedAt, now),
		Href:             runHref,
		WorkflowHref:     actionsWorkflowRoutePath(basePath, row.WorkflowFile),
		WorkflowFileHref: actionsWorkflowFileHref(owner, repoName, codeTarget(row.HeadRef, row.HeadSha), row.WorkflowFile),
		CancelHref:       runHref + "/cancel",
		RerunHref:        runHref + "/rerun",
		CanCancel:        canManage && !workflowRunTerminal(row.Status),
		CanRerun:         canManage && workflowRunTerminal(row.Status),
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

func actionsWorkflowFileFromRoute(r *http.Request) (string, bool) {
	raw := chi.URLParam(r, "*")
	if raw == "" {
		raw = chi.URLParam(r, "file")
	}
	return normalizeActionsWorkflowFile(raw)
}

func normalizeActionsWorkflowFile(file string) (string, bool) {
	file = strings.TrimSpace(file)
	if file == "" || strings.Contains(file, "\x00") || strings.Contains(file, "\\") {
		return "", false
	}
	if strings.HasPrefix(file, "/") {
		return "", false
	}
	file = strings.TrimPrefix(file, dispatch.WorkflowFilesDir)
	if strings.HasPrefix(file, "/") || strings.HasPrefix(file, ".") {
		return "", false
	}
	clean := path.Clean(file)
	if clean == "." || clean != file || strings.HasPrefix(clean, "../") || clean == ".." || strings.Contains(clean, "/../") {
		return "", false
	}
	ext := path.Ext(clean)
	if ext != ".yml" && ext != ".yaml" {
		return "", false
	}
	return dispatch.WorkflowFilesDir + clean, true
}

func actionsWorkflowRoutePath(basePath, workflowFile string) string {
	workflowFile, ok := normalizeActionsWorkflowFile(workflowFile)
	if !ok {
		return ""
	}
	rel := strings.TrimPrefix(workflowFile, dispatch.WorkflowFilesDir)
	if rel == "" {
		return ""
	}
	return basePath + "/workflows/" + escapePathSegments(rel)
}

func actionsWorkflowFileHref(owner, repoName, ref, workflowFile string) string {
	if workflowFile == "" || ref == "" {
		return ""
	}
	workflowFile, ok := normalizeActionsWorkflowFile(workflowFile)
	if !ok {
		return ""
	}
	return "/" + owner + "/" + repoName + "/blob/" + escapePathSegments(ref) + "/" + escapePathSegments(workflowFile)
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

func actionsPagination(basePath string, filters actionsListFilters, total, pageRows int64, includeWorkflow bool) actionsPaginationView {
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
		p := actionsFilterParamsFor(filters, includeWorkflow)
		if filters.Page <= 2 {
			p.Del("page")
		} else {
			p.Set("page", strconv.FormatInt(int64(filters.Page-1), 10))
		}
		view.PrevHref = pathWithQuery(basePath, p)
	}
	if view.HasNext {
		p := actionsFilterParamsFor(filters, includeWorkflow)
		p.Set("page", strconv.FormatInt(int64(filters.Page+1), 10))
		view.NextHref = pathWithQuery(basePath, p)
	}
	return view
}

func actionsFilterParamsFor(filters actionsListFilters, includeWorkflow bool) url.Values {
	v := url.Values{}
	if query := actionsFilterQuery(filters, includeWorkflow); query != "" {
		v.Set("query", query)
	}
	if filters.Page > 1 {
		v.Set("page", strconv.FormatInt(int64(filters.Page), 10))
	}
	return v
}

func actionsFilterQuery(filters actionsListFilters, includeWorkflow bool) string {
	tokens := make([]string, 0, 6)
	if includeWorkflow && filters.Workflow != "" {
		tokens = append(tokens, "workflow:"+quoteActionsFilterValue(filters.Workflow))
	}
	if filters.Branch != "" {
		tokens = append(tokens, "branch:"+quoteActionsFilterValue(filters.Branch))
	}
	if filters.Event != "" {
		tokens = append(tokens, "event:"+quoteActionsFilterValue(filters.Event))
	}
	if filters.Status != "" {
		tokens = append(tokens, "status:"+quoteActionsFilterValue(filters.Status))
	}
	if filters.Conclusion != "" {
		tokens = append(tokens, "conclusion:"+quoteActionsFilterValue(filters.Conclusion))
	}
	if filters.Actor != "" {
		tokens = append(tokens, "actor:"+quoteActionsFilterValue(filters.Actor))
	}
	return strings.Join(tokens, " ")
}

func quoteActionsFilterValue(value string) string {
	if value == "" {
		return value
	}
	if strings.ContainsAny(value, " \t\n\r\"") {
		return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return value
}

func actionsHasClearableFilters(filters actionsListFilters, includeWorkflow bool) bool {
	if includeWorkflow && filters.Workflow != "" {
		return true
	}
	return filters.HasRunFilters
}

func pluralizeInt64(count int64, one, many string) string {
	label := many
	if count == 1 {
		label = one
	}
	return strconv.FormatInt(count, 10) + " " + label
}

func actionsFilterMenus(basePath string, filters actionsListFilters, workflows []actionsWorkflowView, includeWorkflow bool) []actionsFilterMenuView {
	menus := make([]actionsFilterMenuView, 0, 5)
	if includeWorkflow {
		opts := []actionsFilterOption{{
			Label:    "All workflows",
			Selected: filters.Workflow == "",
			Href:     actionsFilterHref(basePath, filters, includeWorkflow, func(f *actionsListFilters) { f.Workflow = "" }),
		}}
		for _, workflow := range workflows {
			opts = append(opts, actionsFilterOption{
				Value:    workflow.File,
				Label:    workflow.Name,
				Selected: filters.Workflow == workflow.File,
				Href:     workflow.Href,
			})
		}
		menus = append(menus, actionsFilterMenuView{Key: "workflow", Label: "Workflow", Options: opts})
	}
	menus = append(menus,
		actionsEventFilterMenu(basePath, filters, includeWorkflow),
		actionsStatusFilterMenu(basePath, filters, includeWorkflow),
		actionsTextFilterMenu("branch", "Branch", "All branches", filters.Branch, basePath, filters, includeWorkflow),
		actionsTextFilterMenu("actor", "Actor", "Anyone", filters.Actor, basePath, filters, includeWorkflow),
	)
	return menus
}

func actionsEventFilterMenu(basePath string, filters actionsListFilters, includeWorkflow bool) actionsFilterMenuView {
	values := []actionsFilterOption{
		{Value: "", Label: "Any event"},
		{Value: "push", Label: "push"},
		{Value: "pull_request", Label: "pull_request"},
		{Value: "schedule", Label: "schedule"},
		{Value: "workflow_dispatch", Label: "workflow_dispatch"},
	}
	for i := range values {
		value := values[i].Value
		values[i].Selected = filters.Event == value
		values[i].Href = actionsFilterHref(basePath, filters, includeWorkflow, func(f *actionsListFilters) { f.Event = value })
	}
	return actionsFilterMenuView{Key: "event", Label: "Event", Options: values}
}

func actionsStatusFilterMenu(basePath string, filters actionsListFilters, includeWorkflow bool) actionsFilterMenuView {
	values := []actionsFilterOption{
		{Value: "", Label: "Any status"},
		{Value: "queued", Label: "queued"},
		{Value: "running", Label: "running"},
		{Value: "completed", Label: "completed"},
		{Value: "cancelled", Label: "cancelled"},
		{Value: "success", Label: "success"},
		{Value: "failure", Label: "failure"},
		{Value: "skipped", Label: "skipped"},
		{Value: "timed_out", Label: "timed_out"},
		{Value: "action_required", Label: "action_required"},
	}
	for i := range values {
		value := values[i].Value
		values[i].Selected = filters.Status == value || filters.Conclusion == value || (value == "" && filters.Status == "" && filters.Conclusion == "")
		values[i].Href = actionsFilterHref(basePath, filters, includeWorkflow, func(f *actionsListFilters) {
			f.Status = ""
			f.Conclusion = ""
			if status := validWorkflowRunStatus(value); status != "" {
				f.Status = status
			} else if conclusion := validWorkflowRunConclusion(value); conclusion != "" {
				f.Conclusion = conclusion
			}
		})
	}
	return actionsFilterMenuView{Key: "status", Label: "Status", Options: values}
}

func actionsTextFilterMenu(key, label, allLabel, selected string, basePath string, filters actionsListFilters, includeWorkflow bool) actionsFilterMenuView {
	opts := []actionsFilterOption{{
		Label:    allLabel,
		Selected: selected == "",
		Href: actionsFilterHref(basePath, filters, includeWorkflow, func(f *actionsListFilters) {
			if key == "branch" {
				f.Branch = ""
			} else {
				f.Actor = ""
			}
		}),
	}}
	if selected != "" {
		opts = append(opts, actionsFilterOption{Value: selected, Label: selected, Selected: true, Href: pathWithQuery(basePath, actionsFilterParamsFor(filters, includeWorkflow))})
	} else {
		opts = append(opts, actionsFilterOption{Label: "Use " + key + ": in the filter bar", Disabled: true})
	}
	return actionsFilterMenuView{Key: key, Label: label, Options: opts}
}

func actionsFilterHref(basePath string, filters actionsListFilters, includeWorkflow bool, mutate func(*actionsListFilters)) string {
	next := filters
	next.Page = 1
	if mutate != nil {
		mutate(&next)
	}
	next.HasRunFilters = next.Branch != "" || next.Event != "" || next.Status != "" || next.Conclusion != "" || next.Actor != ""
	next.HasAny = next.Workflow != "" || next.HasRunFilters
	return pathWithQuery(basePath, actionsFilterParamsFor(next, includeWorkflow))
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
	data["RunGraphJSON"] = actionsRunGraphJSON(view.Graph)
	data["UseHTMX"] = true
	data["UseActionsGraphJS"] = true
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
	annotations, warningCount, errorCount, err := h.loadStepAnnotations(r.Context(), step.ID, owner.Username, row.Name, run, step)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: load step annotations", "step_id", step.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	view := actionsStepLogView{
		Run:          run,
		Job:          job,
		Step:         step,
		Annotations:  annotations,
		WarningCount: warningCount,
		ErrorCount:   errorCount,
		LogText:      logContent.Text,
		LogSource:    logContent.Source,
		LogError:     logContent.Error,
		LogTruncated: logContent.Truncated,
		BackHref:     run.ActionsHref + "/runs/" + strconv.FormatInt(run.RunIndex, 10) + "#job-" + strconv.FormatInt(int64(job.JobIndex), 10),
	}
	if logContent.HasLog && logContent.Error == "" {
		view.DownloadHref = step.LogHref + "/log/download"
	}
	if !step.IsTerminal && logContent.Error == "" && logContent.CanStream {
		view.StreamHref = step.LogHref + "/log/stream?after=" + strconv.FormatInt(int64(logContent.LastSeq), 10)
	}
	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = step.Name + " · " + run.Title + " #" + strconv.FormatInt(run.RunIndex, 10)
	data["Log"] = view
	if err := h.d.Render.RenderPage(w, r, "repo/action_step_log", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo action step log render", "run_index", runIndex, "job_index", jobIndex, "step_index", stepIndex, "error", err)
	}
}

func (h *Handlers) repoActionStepLogDownload(w http.ResponseWriter, r *http.Request) {
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
			h.d.Logger.WarnContext(r.Context(), "repo actions: get run for log download", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	job, step, ok := findActionStep(run, jobIndex, stepIndex)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	filename := actionsStepLogFilename(run.RunIndex, job.JobIndex, step.StepIndex)
	if err := h.writeStepLogDownload(r.Context(), w, step.ID, filename); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		if errors.Is(err, errActionsLogStorageUnavailable) {
			h.d.Render.HTTPError(w, r, http.StatusServiceUnavailable, "")
			return
		}
		h.d.Logger.WarnContext(r.Context(), "repo actions: stream log download", "step_id", step.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
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
	view.Graph = actionsRunGraph(view.Jobs)
	annotations, err := q.ListWorkflowAnnotationsForRun(ctx, h.d.Pool, run.ID)
	if err != nil {
		return actionsRunDetailView{}, err
	}
	view.AnnotationGroups, view.AnnotationCount, view.WarningCount, view.ErrorCount = actionsAnnotationGroups(annotations, owner, repoName, run.RunIndex, codeTarget(run.HeadRef, run.HeadSha))
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

const (
	actionsRunGraphNodeWidth  = 240
	actionsRunGraphNodeHeight = 76
	actionsRunGraphColumnGap  = 96
	actionsRunGraphRowGap     = 28
	actionsRunGraphMarginX    = 32
	actionsRunGraphMarginY    = 32
)

func actionsRunGraph(jobs []actionsJobDetailView) actionsRunGraphView {
	graph := actionsRunGraphView{
		NodeWidth:  actionsRunGraphNodeWidth,
		NodeHeight: actionsRunGraphNodeHeight,
		Nodes:      []actionsRunGraphNodeView{},
		Edges:      []actionsRunGraphEdgeView{},
	}
	if len(jobs) == 0 {
		graph.CanvasWidth = actionsRunGraphMarginX * 2
		graph.CanvasHeight = actionsRunGraphMarginY * 2
		return graph
	}

	stages := actionsJobStages(jobs)
	maxRows := 1
	for _, stage := range stages {
		if len(stage.Jobs) > maxRows {
			maxRows = len(stage.Jobs)
		}
	}
	graph.CanvasWidth = actionsRunGraphMarginX*2 + len(stages)*actionsRunGraphNodeWidth
	if len(stages) > 1 {
		graph.CanvasWidth += (len(stages) - 1) * actionsRunGraphColumnGap
	}
	graph.CanvasHeight = actionsRunGraphMarginY*2 + maxRows*actionsRunGraphNodeHeight
	if maxRows > 1 {
		graph.CanvasHeight += (maxRows - 1) * actionsRunGraphRowGap
	}

	nodesByJobKey := make(map[string]actionsRunGraphNodeView, len(jobs))
	for _, stage := range stages {
		for rowIndex, job := range stage.Jobs {
			node := actionsRunGraphNode(job, stage.Index, rowIndex)
			graph.Nodes = append(graph.Nodes, node)
			nodesByJobKey[job.JobKey] = node
		}
	}
	for _, node := range graph.Nodes {
		for _, need := range node.Needs {
			from, ok := nodesByJobKey[need]
			if !ok {
				continue
			}
			graph.Edges = append(graph.Edges, actionsRunGraphEdge(from, node))
		}
	}
	return graph
}

func actionsRunGraphNode(job actionsJobDetailView, stageIndex, rowIndex int) actionsRunGraphNodeView {
	node := actionsRunGraphNodeView{
		ID:         "job-" + strconv.FormatInt(int64(job.JobIndex), 10),
		JobIndex:   job.JobIndex,
		JobKey:     job.JobKey,
		Name:       job.Name,
		RunsOn:     job.RunsOn,
		Needs:      append([]string(nil), job.Needs...),
		NeedsText:  job.NeedsText,
		StateText:  job.StateText,
		StateClass: job.StateClass,
		StateIcon:  job.StateIcon,
		Duration:   job.Duration,
		Anchor:     job.Anchor,
		X:          actionsRunGraphMarginX + stageIndex*(actionsRunGraphNodeWidth+actionsRunGraphColumnGap),
		Y:          actionsRunGraphMarginY + rowIndex*(actionsRunGraphNodeHeight+actionsRunGraphRowGap),
		Width:      actionsRunGraphNodeWidth,
		Height:     actionsRunGraphNodeHeight,
		StepCount:  len(job.Steps),
		Steps:      make([]actionsRunGraphStepView, 0, len(job.Steps)),
	}
	for _, step := range job.Steps {
		if step.IsTerminal {
			node.CompletedStepCount++
		}
		if step.StateClass == "failure" {
			node.FailureCount++
		}
		node.Steps = append(node.Steps, actionsRunGraphStepView{
			Name:       step.Name,
			Kind:       step.Kind,
			Detail:     step.Detail,
			StateText:  step.StateText,
			StateClass: step.StateClass,
			Duration:   step.Duration,
			LogHref:    step.LogHref,
		})
	}
	return node
}

func actionsRunGraphEdge(from, to actionsRunGraphNodeView) actionsRunGraphEdgeView {
	x1 := from.X + from.Width
	y1 := from.Y + from.Height/2
	x2 := to.X
	y2 := to.Y + to.Height/2
	controlGap := 48
	if x2 > x1 {
		controlGap = (x2 - x1) / 2
		if controlGap < 48 {
			controlGap = 48
		}
	}
	return actionsRunGraphEdgeView{
		From: from.ID,
		To:   to.ID,
		Path: fmt.Sprintf("M%d %d C%d %d %d %d %d %d", x1, y1, x1+controlGap, y1, x2-controlGap, y2, x2, y2),
	}
}

func actionsRunGraphJSON(graph actionsRunGraphView) template.JS {
	raw, err := json.Marshal(graph)
	if err != nil {
		return template.JS(`{"canvasWidth":0,"canvasHeight":0,"nodeWidth":0,"nodeHeight":0,"nodes":[],"edges":[]}`) //nolint:gosec // constant fallback
	}
	return template.JS(raw) //nolint:gosec // json.Marshal escapes script-breaking characters
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

func actionsAnnotationGroups(rows []actionsdb.ListWorkflowAnnotationsForRunRow, owner, repoName string, runIndex int64, sourceTarget string) ([]actionsAnnotationGroupView, int, int, int) {
	groups := make([]actionsAnnotationGroupView, 0)
	count, warnings, errorsCount := 0, 0, 0
	for _, row := range rows {
		if len(groups) == 0 || groups[len(groups)-1].JobID != row.JobID {
			name := strings.TrimSpace(row.JobName)
			if name == "" {
				name = row.JobKey
			}
			groups = append(groups, actionsAnnotationGroupView{
				JobID:     row.JobID,
				JobName:   name,
				JobAnchor: "job-" + strconv.FormatInt(int64(row.JobIndex), 10),
			})
		}
		view := actionsAnnotationViewFromRunRow(row, owner, repoName, runIndex, sourceTarget)
		count++
		switch view.Level {
		case string(actionsdb.WorkflowAnnotationLevelError):
			errorsCount++
		case string(actionsdb.WorkflowAnnotationLevelWarning):
			warnings++
		}
		last := &groups[len(groups)-1]
		last.Count++
		last.Annotations = append(last.Annotations, view)
	}
	return groups, count, warnings, errorsCount
}

func actionsAnnotationViewFromRunRow(row actionsdb.ListWorkflowAnnotationsForRunRow, owner, repoName string, runIndex int64, sourceTarget string) actionsAnnotationView {
	stepName := actionsAnnotationStepName(row.StepName, row.RunCommand, row.UsesAlias, row.StepIndex)
	stepHref := "/" + owner + "/" + repoName + "/actions/runs/" + strconv.FormatInt(runIndex, 10) +
		"/jobs/" + strconv.FormatInt(int64(row.JobIndex), 10) +
		"/steps/" + strconv.FormatInt(int64(row.StepIndex), 10)
	view := actionsAnnotationView{
		ID:         row.ID,
		Level:      string(row.Level),
		Title:      row.Title,
		Message:    row.Message,
		Path:       row.Path,
		StepName:   stepName,
		StepHref:   stepHref,
		Location:   actionsAnnotationLocation(row.Path, row.StartLine, row.StartColumn),
		SourceHref: actionsAnnotationSourceHref(owner, repoName, sourceTarget, row.Path, row.StartLine),
	}
	view.LevelLabel, view.StateClass, view.Icon = actionsAnnotationPresentation(view.Level)
	return view
}

func actionsAnnotationViewFromStepRow(row actionsdb.WorkflowAnnotation, owner, repoName, sourceTarget string, step actionsStepDetailView) actionsAnnotationView {
	view := actionsAnnotationView{
		ID:         row.ID,
		Level:      string(row.Level),
		Title:      row.Title,
		Message:    row.Message,
		Path:       row.Path,
		StepName:   step.Name,
		StepHref:   step.LogHref,
		Location:   actionsAnnotationLocation(row.Path, row.StartLine, row.StartColumn),
		SourceHref: actionsAnnotationSourceHref(owner, repoName, sourceTarget, row.Path, row.StartLine),
	}
	view.LevelLabel, view.StateClass, view.Icon = actionsAnnotationPresentation(view.Level)
	return view
}

func actionsAnnotationStepName(stepName, runCommand, usesAlias string, stepIndex int32) string {
	if name := strings.TrimSpace(stepName); name != "" {
		return name
	}
	if alias := strings.TrimSpace(usesAlias); alias != "" {
		return alias
	}
	if line := firstCommandLine(runCommand); line != "" {
		return line
	}
	return "Step " + strconv.Itoa(int(stepIndex)+1)
}

func actionsAnnotationPresentation(level string) (label, stateClass, icon string) {
	switch level {
	case string(actionsdb.WorkflowAnnotationLevelError):
		return "Error", "failure", "x-circle"
	case string(actionsdb.WorkflowAnnotationLevelNotice):
		return "Notice", "neutral", "dot-fill"
	default:
		return "Warning", "pending", "alert"
	}
}

func actionsAnnotationLocation(filePath string, line, column pgtype.Int4) string {
	filePath = strings.TrimSpace(filePath)
	if filePath == "" {
		return ""
	}
	if line.Valid {
		if column.Valid {
			return filePath + ":" + strconv.FormatInt(int64(line.Int32), 10) + ":" + strconv.FormatInt(int64(column.Int32), 10)
		}
		return filePath + ":" + strconv.FormatInt(int64(line.Int32), 10)
	}
	return filePath
}

func actionsAnnotationSourceHref(owner, repoName, target, filePath string, line pgtype.Int4) string {
	if target == "" {
		return ""
	}
	clean, ok := cleanAnnotationPath(filePath)
	if !ok {
		return ""
	}
	href := "/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/blob/" + url.PathEscape(target) + "/" + escapePathSegments(clean)
	if line.Valid {
		href += "#L" + strconv.FormatInt(int64(line.Int32), 10)
	}
	return href
}

func cleanAnnotationPath(filePath string) (string, bool) {
	filePath = strings.ReplaceAll(strings.TrimSpace(filePath), "\\", "/")
	if filePath == "" || strings.ContainsRune(filePath, '\x00') || strings.HasPrefix(filePath, "/") {
		return "", false
	}
	clean := path.Clean(filePath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func (h *Handlers) loadStepAnnotations(ctx context.Context, stepID int64, owner, repoName string, run actionsRunDetailView, step actionsStepDetailView) ([]actionsAnnotationView, int, int, error) {
	rows, err := actionsdb.New().ListWorkflowAnnotationsForStep(ctx, h.d.Pool, stepID)
	if err != nil {
		return nil, 0, 0, err
	}
	out := make([]actionsAnnotationView, 0, len(rows))
	warnings, errorsCount := 0, 0
	for _, row := range rows {
		view := actionsAnnotationViewFromStepRow(row, owner, repoName, codeTarget(run.HeadRef, run.HeadSha), step)
		switch view.Level {
		case string(actionsdb.WorkflowAnnotationLevelError):
			errorsCount++
		case string(actionsdb.WorkflowAnnotationLevelWarning):
			warnings++
		}
		out = append(out, view)
	}
	return out, warnings, errorsCount, nil
}

type actionsStepLogContent struct {
	Text      string
	Source    string
	Error     string
	Truncated bool
	LastSeq   int32
	HasLog    bool
	CanStream bool
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
		HasLog:    lastSeq >= 0 || buf.Len() > 0,
		CanStream: true,
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
	return actionsStepLogContent{
		Text:      strings.ToValidUTF8(string(body), "\uFFFD"),
		Source:    "object storage",
		Truncated: truncated,
		HasLog:    true,
	}, nil
}

func actionsStepLogFilename(runIndex int64, jobIndex, stepIndex int32) string {
	return "shithub-run-" + strconv.FormatInt(runIndex, 10) +
		"-job-" + strconv.FormatInt(int64(jobIndex), 10) +
		"-step-" + strconv.FormatInt(int64(stepIndex), 10) + ".log"
}

func (h *Handlers) writeStepLogDownload(ctx context.Context, w http.ResponseWriter, stepID int64, filename string) error {
	q := actionsdb.New()
	step, err := q.GetWorkflowStepByID(ctx, h.d.Pool, stepID)
	if err != nil {
		return err
	}
	if step.LogObjectKey.Valid && step.LogObjectKey.String != "" {
		if h.d.ObjectStore == nil {
			return errActionsLogStorageUnavailable
		}
		rc, _, err := h.d.ObjectStore.Get(ctx, step.LogObjectKey.String)
		if err != nil {
			return err
		}
		defer rc.Close()
		writeStepLogDownloadHeaders(w, filename)
		_, err = io.Copy(w, rc)
		return err
	}

	const batchSize = int32(128)
	afterSeq := int32(-1)
	wroteHeaders := false
	for {
		chunks, err := q.ListStepLogChunks(ctx, h.d.Pool, actionsdb.ListStepLogChunksParams{
			StepID: step.ID,
			Seq:    afterSeq,
			Limit:  batchSize,
		})
		if err != nil {
			return err
		}
		if !wroteHeaders {
			writeStepLogDownloadHeaders(w, filename)
			wroteHeaders = true
		}
		if len(chunks) == 0 {
			return nil
		}
		for _, chunk := range chunks {
			if _, err := w.Write(chunk.Chunk); err != nil {
				return err
			}
			afterSeq = chunk.Seq
		}
		if len(chunks) < int(batchSize) {
			return nil
		}
	}
}

func writeStepLogDownloadHeaders(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
