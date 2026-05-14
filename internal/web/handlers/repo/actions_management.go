// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

const actionsManagementListLimit = int32(50)

type actionsManagementPageView struct {
	Key               string
	Title             string
	Description       string
	Icon              string
	SearchPlaceholder string
	CountLabel        string
	EmptyTitle        string
	EmptyBody         string
	PrimaryAction     string
	PrimaryDisabled   bool
	PeriodLabel       string
	FooterNote        string
	Stats             []actionsManagementStatView
	Tabs              []actionsManagementTabView
	Caches            []actionsCacheRowView
	Runners           []actionsRunnerRowView
	UsageRows         []actionsUsageWorkflowRowView
	PerformanceRows   []actionsPerformanceWorkflowRowView
	HasRows           bool
}

type actionsManagementStatView struct {
	Label string
	Value string
	Note  string
}

type actionsManagementTabView struct {
	Label  string
	Icon   string
	Active bool
}

type actionsCacheRowView struct {
	Key      string
	Version  string
	Ref      string
	Size     string
	LastUsed string
	Created  string
}

type actionsRunnerRowView struct {
	Name           string
	Labels         []string
	Status         string
	StateClass     string
	LastHeartbeat  string
	Capacity       string
	ActiveJobCount string
	Draining       bool
}

type actionsUsageWorkflowRowView struct {
	WorkflowName string
	WorkflowFile string
	RunCount     string
	JobCount     string
	Minutes      string
	Duration     string
}

type actionsPerformanceWorkflowRowView struct {
	WorkflowName string
	WorkflowFile string
	RunCount     string
	JobCount     string
	AvgRuntime   string
	AvgQueue     string
	FailureRate  string
}

func (h *Handlers) repoActionsCaches(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, "caches")
}

func (h *Handlers) repoActionsAttestations(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, "attestations")
}

func (h *Handlers) repoActionsRunners(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, "runners")
}

func (h *Handlers) repoActionsUsageMetrics(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, "usage")
}

func (h *Handlers) repoActionsPerformanceMetrics(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, "performance")
}

func (h *Handlers) repoActionsManagementPage(w http.ResponseWriter, r *http.Request, key string) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}

	page := actionsManagementPage(key)
	q := actionsdb.New()
	workflowRows, err := q.ListWorkflowRunWorkflowsForRepo(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list workflows for management page", "repo_id", row.ID, "page", page.Key, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.populateActionsManagementPage(r.Context(), row.ID, &page, time.Now()); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: load management page data", "repo_id", row.ID, "page", page.Key, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	basePath := "/" + owner.Username + "/" + row.Name + "/actions"
	workflows, allRunCount, _ := actionsWorkflowViews(workflowRows, actionsListFilters{}, basePath)

	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = page.Title + " · " + row.Name
	data["Page"] = page
	data["ActionsSidebar"] = actionsSidebar(basePath, workflows, allRunCount, false, page.Key)
	data["RunCount"] = allRunCount
	data["Workflows"] = workflows
	if err := h.d.Render.RenderPage(w, r, "repo/actions_management", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo actions management render", "page", page.Key, "error", err)
	}
}

func (h *Handlers) populateActionsManagementPage(ctx context.Context, repoID int64, page *actionsManagementPageView, now time.Time) error {
	q := actionsdb.New()
	switch page.Key {
	case "caches":
		total, err := q.CountWorkflowCachesForRepo(ctx, h.d.Pool, actionsdb.CountWorkflowCachesForRepoParams{RepoID: repoID})
		if err != nil {
			return err
		}
		rows, err := q.ListWorkflowCachesForRepo(ctx, h.d.Pool, actionsdb.ListWorkflowCachesForRepoParams{
			RepoID: repoID,
			Limit:  actionsManagementListLimit,
			Offset: 0,
		})
		if err != nil {
			return err
		}
		page.CountLabel = pluralCount(total, "cache", "caches")
		page.Caches = actionsCacheRows(rows, now)
		page.HasRows = len(page.Caches) > 0
		if total > int64(len(page.Caches)) {
			page.FooterNote = "Showing the first " + strconv.Itoa(len(page.Caches)) + " caches."
		}
	case "runners":
		rows, err := q.ListRunnersForRepo(ctx, h.d.Pool, repoID)
		if err != nil {
			return err
		}
		page.Runners = actionsRunnerRows(rows, now)
		page.HasRows = len(page.Runners) > 0
		page.CountLabel = pluralCount(int64(len(page.Runners)), "matching runner", "matching runners")
		if !page.HasRows {
			page.EmptyTitle = "No matching runners"
			page.EmptyBody = "No registered runner currently matches labels used by this repository's workflow jobs."
		}
	case "usage":
		since := currentActionsMetricsPeriodStart(now)
		page.PeriodLabel = "Showing data from " + since.Format("Jan 2, 2006") + " to now."
		summary, err := q.GetActionsUsageSummaryForRepo(ctx, h.d.Pool, actionsdb.GetActionsUsageSummaryForRepoParams{
			RepoID:    repoID,
			CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
		})
		if err != nil {
			return err
		}
		rows, err := q.ListActionsUsageWorkflowsForRepo(ctx, h.d.Pool, actionsdb.ListActionsUsageWorkflowsForRepoParams{
			RepoID:    repoID,
			CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
			Limit:     actionsManagementListLimit,
		})
		if err != nil {
			return err
		}
		page.Stats = []actionsManagementStatView{
			{Label: "Total minutes", Value: formatActionsMinutes(summary.CompletedJobSeconds), Note: "Completed job duration across workflows in this repository."},
			{Label: "Total workflow runs", Value: strconv.FormatInt(summary.RunCount, 10), Note: "Workflow runs created in this repository for the current period."},
			{Label: "Total job runs", Value: strconv.FormatInt(summary.JobCount, 10), Note: "Jobs created in this repository for the current period."},
		}
		page.UsageRows = actionsUsageRows(rows)
		page.HasRows = len(page.UsageRows) > 0
		page.CountLabel = pluralCount(int64(len(page.UsageRows)), "workflow", "workflows")
	case "performance":
		since := currentActionsMetricsPeriodStart(now)
		page.PeriodLabel = "Showing data from " + since.Format("Jan 2, 2006") + " to now."
		summary, err := q.GetActionsPerformanceSummaryForRepo(ctx, h.d.Pool, actionsdb.GetActionsPerformanceSummaryForRepoParams{
			RepoID:    repoID,
			CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
		})
		if err != nil {
			return err
		}
		rows, err := q.ListActionsPerformanceWorkflowsForRepo(ctx, h.d.Pool, actionsdb.ListActionsPerformanceWorkflowsForRepoParams{
			RepoID:    repoID,
			CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
			Limit:     actionsManagementListLimit,
		})
		if err != nil {
			return err
		}
		page.Stats = []actionsManagementStatView{
			{Label: "Avg job run time", Value: formatMetricSeconds(summary.AvgJobSeconds), Note: "Average completed job runtime in this repository."},
			{Label: "Avg job queue time", Value: formatMetricSeconds(summary.AvgQueueSeconds), Note: "Average time from job creation to runner start."},
			{Label: "Job failure rate", Value: formatPercent(summary.FailedJobCount, summary.TerminalJobCount), Note: "Failed or cancelled terminal jobs in this repository."},
			{Label: "Failed job usage", Value: formatActionsMinutes(summary.FailedJobSeconds), Note: "Completed minutes used by failed or cancelled jobs."},
		}
		page.PerformanceRows = actionsPerformanceRows(rows)
		page.HasRows = len(page.PerformanceRows) > 0
		page.CountLabel = pluralCount(int64(len(page.PerformanceRows)), "workflow", "workflows")
	}
	return nil
}

func actionsManagementPage(key string) actionsManagementPageView {
	switch key {
	case "caches":
		return actionsManagementPageView{
			Key:               key,
			Title:             "Caches",
			Description:       "Showing caches from all workflows.",
			Icon:              "cache",
			SearchPlaceholder: "Filter caches",
			CountLabel:        "0 caches",
			EmptyTitle:        "No caches",
			EmptyBody:         "Nothing has been cached by workflows running in this repository.",
		}
	case "attestations":
		return actionsManagementPageView{
			Key:               key,
			Title:             "Attestations",
			Description:       "Build provenance and artifact attestations for this repository.",
			Icon:              "shield-check",
			SearchPlaceholder: "Search or filter",
			CountLabel:        "0 attestations",
			EmptyTitle:        "No attestations",
			EmptyBody:         "No workflow has published an artifact attestation for this repository yet.",
		}
	case "runners":
		return actionsManagementPageView{
			Key:               key,
			Title:             "Runners",
			Description:       "Runners available to this repository.",
			Icon:              "server",
			SearchPlaceholder: "Filter runners",
			CountLabel:        "0 matching runners",
			EmptyTitle:        "No matching runners",
			EmptyBody:         "No registered runner currently matches labels used by this repository's workflow jobs.",
			PrimaryAction:     "New runner",
			PrimaryDisabled:   true,
			Tabs: []actionsManagementTabView{
				{Label: "shithub-hosted runners", Icon: "server", Active: true},
				{Label: "Self-hosted runners", Icon: "repo"},
			},
		}
	case "usage":
		return actionsManagementPageView{
			Key:         key,
			Title:       "Actions Usage Metrics",
			Description: "Showing data for the current billing period.",
			Icon:        "pulse",
			CountLabel:  "No table data available yet",
			EmptyTitle:  "No usage metrics available yet",
			EmptyBody:   "Workflow minute and job-run aggregates will appear here after the Actions metering model is wired into the repository UI.",
			Stats: []actionsManagementStatView{
				{Label: "Total minutes", Value: "0", Note: "Total minutes across all workflows in this repository."},
				{Label: "Total job runs", Value: "0", Note: "Total job runs across all workflows in this repository."},
			},
			Tabs: metricTabs("workflows"),
		}
	case "performance":
		return actionsManagementPageView{
			Key:         key,
			Title:       "Actions Performance Metrics",
			Description: "Showing data for the current month.",
			Icon:        "stopwatch",
			CountLabel:  "No table data available yet",
			EmptyTitle:  "No performance metrics available yet",
			EmptyBody:   "Average runtime, queue time, and failure-rate metrics will appear here once the insights pipeline is connected to the Actions UI.",
			Stats: []actionsManagementStatView{
				{Label: "Avg job run time", Value: "0s", Note: "Average run time of jobs in this repository."},
				{Label: "Avg job queue time", Value: "0s", Note: "Average queue time of jobs in this repository."},
				{Label: "Job failure rate", Value: "0%", Note: "Failure rate across jobs in this repository."},
				{Label: "Failed job usage", Value: "0", Note: "Total minutes used by failed jobs."},
			},
			Tabs: metricTabs("workflows"),
		}
	default:
		return actionsManagementPageView{Key: key, Title: "Actions", EmptyTitle: "Coming later"}
	}
}

func actionsCacheRows(rows []actionsdb.WorkflowCache, now time.Time) []actionsCacheRowView {
	out := make([]actionsCacheRowView, 0, len(rows))
	for _, row := range rows {
		out = append(out, actionsCacheRowView{
			Key:      row.CacheKey,
			Version:  row.CacheVersion,
			Ref:      shortActionsRef(row.GitRef),
			Size:     formatByteSize(row.SizeBytes),
			LastUsed: formatPGRelative(row.LastAccessedAt, now, "Never"),
			Created:  formatPGRelative(row.CreatedAt, now, "Unknown"),
		})
	}
	return out
}

func actionsRunnerRows(rows []actionsdb.ListRunnersForRepoRow, now time.Time) []actionsRunnerRowView {
	out := make([]actionsRunnerRowView, 0, len(rows))
	for _, row := range rows {
		status, stateClass := actionsRunnerStatus(row)
		out = append(out, actionsRunnerRowView{
			Name:           row.Name,
			Labels:         append([]string(nil), row.Labels...),
			Status:         status,
			StateClass:     stateClass,
			LastHeartbeat:  formatPGRelative(row.LastHeartbeatAt, now, "Never"),
			Capacity:       strconv.FormatInt(int64(row.Capacity), 10),
			ActiveJobCount: strconv.FormatInt(int64(row.ActiveJobCount), 10),
			Draining:       row.DrainingAt.Valid,
		})
	}
	return out
}

func actionsRunnerStatus(row actionsdb.ListRunnersForRepoRow) (string, string) {
	if row.DrainingAt.Valid {
		return "Draining", "pending"
	}
	switch row.Status {
	case actionsdb.WorkflowRunnerStatusBusy:
		return "Busy", "pending"
	case actionsdb.WorkflowRunnerStatusIdle:
		return "Idle", "success"
	case actionsdb.WorkflowRunnerStatusOffline:
		return "Offline", "neutral"
	default:
		return titleToken(string(row.Status)), "neutral"
	}
}

func actionsUsageRows(rows []actionsdb.ListActionsUsageWorkflowsForRepoRow) []actionsUsageWorkflowRowView {
	out := make([]actionsUsageWorkflowRowView, 0, len(rows))
	for _, row := range rows {
		out = append(out, actionsUsageWorkflowRowView{
			WorkflowName: row.WorkflowName,
			WorkflowFile: row.WorkflowFile,
			RunCount:     strconv.FormatInt(row.RunCount, 10),
			JobCount:     strconv.FormatInt(row.JobCount, 10),
			Minutes:      formatActionsMinutes(row.CompletedJobSeconds),
			Duration:     formatMetricSeconds(float64(row.CompletedJobSeconds)),
		})
	}
	return out
}

func actionsPerformanceRows(rows []actionsdb.ListActionsPerformanceWorkflowsForRepoRow) []actionsPerformanceWorkflowRowView {
	out := make([]actionsPerformanceWorkflowRowView, 0, len(rows))
	for _, row := range rows {
		out = append(out, actionsPerformanceWorkflowRowView{
			WorkflowName: row.WorkflowName,
			WorkflowFile: row.WorkflowFile,
			RunCount:     strconv.FormatInt(row.RunCount, 10),
			JobCount:     strconv.FormatInt(row.JobCount, 10),
			AvgRuntime:   formatMetricSeconds(row.AvgJobSeconds),
			AvgQueue:     formatMetricSeconds(row.AvgQueueSeconds),
			FailureRate:  formatPercent(row.FailedJobCount, row.TerminalJobCount),
		})
	}
	return out
}

func metricTabs(active string) []actionsManagementTabView {
	tabs := []actionsManagementTabView{
		{Label: "Workflows", Icon: "workflow"},
		{Label: "Jobs", Icon: "stopwatch"},
		{Label: "Runtime OS", Icon: "server"},
		{Label: "Runner type", Icon: "repo"},
	}
	for i := range tabs {
		tabs[i].Active = active == "workflows" && i == 0
	}
	return tabs
}

func currentActionsMetricsPeriodStart(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func shortActionsRef(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.TrimPrefix(ref, "refs/heads/")
	ref = strings.TrimPrefix(ref, "refs/tags/")
	if ref == "" {
		return "-"
	}
	return ref
}

func formatPGRelative(ts pgtype.Timestamptz, now time.Time, fallback string) string {
	if !ts.Valid || ts.Time.IsZero() {
		return fallback
	}
	return formatRelativeTime(ts.Time, now)
}

func formatRelativeTime(t, now time.Time) string {
	if t.After(now) {
		return "just now"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	case d < 30*24*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
	default:
		return t.Format("Jan 2, 2006")
	}
}

func formatByteSize(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	const unit = 1024
	if bytes < unit {
		return strconv.FormatInt(bytes, 10) + " B"
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit
		if value < unit {
			if value >= 10 || math.Mod(value, 1) == 0 {
				return strconv.FormatFloat(value, 'f', 0, 64) + " " + suffix
			}
			return strconv.FormatFloat(value, 'f', 1, 64) + " " + suffix
		}
	}
	return strconv.FormatFloat(value/unit, 'f', 0, 64) + " EiB"
}

func formatActionsMinutes(seconds int64) string {
	if seconds <= 0 {
		return "0"
	}
	return strconv.FormatInt((seconds+59)/60, 10)
}

func formatMetricSeconds(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	if seconds < 1 {
		return "<1s"
	}
	return formatDuration(time.Duration(math.Round(seconds)) * time.Second)
}

func formatPercent(numerator, denominator int64) string {
	if denominator <= 0 || numerator <= 0 {
		return "0%"
	}
	percent := float64(numerator) * 100 / float64(denominator)
	if percent >= 10 || math.Mod(percent, 1) == 0 {
		return strconv.FormatFloat(percent, 'f', 0, 64) + "%"
	}
	return strconv.FormatFloat(percent, 'f', 1, 64) + "%"
}

func pluralCount(count int64, singular, plural string) string {
	label := plural
	if count == 1 {
		label = singular
	}
	return strconv.FormatInt(count, 10) + " " + label
}

func titleToken(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "_", " "))
	if value == "" {
		return "Unknown"
	}
	parts := strings.Fields(value)
	for i, part := range parts {
		if len(part) == 0 {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}
