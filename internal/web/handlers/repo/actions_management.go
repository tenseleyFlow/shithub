// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"net/http"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
)

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
	Stats             []actionsManagementStatView
	Tabs              []actionsManagementTabView
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

func (h *Handlers) repoActionsCaches(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, actionsManagementPage("caches"))
}

func (h *Handlers) repoActionsAttestations(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, actionsManagementPage("attestations"))
}

func (h *Handlers) repoActionsRunners(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, actionsManagementPage("runners"))
}

func (h *Handlers) repoActionsUsageMetrics(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, actionsManagementPage("usage"))
}

func (h *Handlers) repoActionsPerformanceMetrics(w http.ResponseWriter, r *http.Request) {
	h.repoActionsManagementPage(w, r, actionsManagementPage("performance"))
}

func (h *Handlers) repoActionsManagementPage(w http.ResponseWriter, r *http.Request, page actionsManagementPageView) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}

	q := actionsdb.New()
	workflowRows, err := q.ListWorkflowRunWorkflowsForRepo(r.Context(), h.d.Pool, row.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list workflows for management page", "repo_id", row.ID, "page", page.Key, "error", err)
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
			CountLabel:        "Runner management",
			EmptyTitle:        "Repository runner management is coming later",
			EmptyBody:         "Jobs can already use the shared shithub runner pool. Repository-scoped runner registration controls will land in a later Actions slice.",
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
