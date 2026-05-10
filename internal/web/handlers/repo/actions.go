// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	checksdb "github.com/tenseleyFlow/shithub/internal/checks/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type actionsWorkflowView struct {
	Name  string
	Count int
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
	suiteRows, err := h.cq.ListCheckSuitesForRepo(r.Context(), h.d.Pool, checksdb.ListCheckSuitesForRepoParams{
		RepoID: row.ID,
		Limit:  50,
		Offset: 0,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list suites", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	suites := make([]actionsSuiteView, 0, len(suiteRows))
	workflowCounts := map[string]int{}
	workflowOrder := []string{}
	for _, suite := range suiteRows {
		runs, err := h.cq.ListCheckRunsBySuite(r.Context(), h.d.Pool, suite.ID)
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "repo actions: list runs", "suite_id", suite.ID, "error", err)
			continue
		}
		if _, ok := workflowCounts[suite.AppSlug]; !ok {
			workflowOrder = append(workflowOrder, suite.AppSlug)
		}
		workflowCounts[suite.AppSlug]++
		suites = append(suites, actionsSuiteViewFromListRow(suite, runs))
	}

	workflows := make([]actionsWorkflowView, 0, len(workflowOrder))
	for _, name := range workflowOrder {
		workflows = append(workflows, actionsWorkflowView{Name: name, Count: workflowCounts[name]})
	}

	data := h.repoHeaderData(r, row, owner.Username, "actions")
	data["Title"] = "Actions · " + row.Name
	data["Suites"] = suites
	data["Workflows"] = workflows
	data["RunCount"] = len(suites)
	if err := h.d.Render.RenderPage(w, r, "repo/actions", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "repo actions render", "error", err)
	}
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

func actionsSuiteViewFromListRow(row checksdb.ListCheckSuitesForRepoRow, runs []checksdb.CheckRun) actionsSuiteView {
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
	stateText, stateClass, stateIcon := actionState(status, conclusion)
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
	stateText, stateClass, stateIcon := actionState(run.Status, run.Conclusion)
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

func actionState(status checksdb.CheckStatus, conclusion checksdb.NullCheckConclusion) (string, string, string) {
	if !conclusion.Valid {
		switch status {
		case checksdb.CheckStatusCompleted:
			return "Completed", "neutral", "check-circle"
		case checksdb.CheckStatusInProgress:
			return "In progress", "pending", "dot-fill"
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
