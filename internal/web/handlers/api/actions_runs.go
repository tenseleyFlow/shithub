// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsRuns registers the S50 §18 actions-runs REST surface.
//
//	GET /api/v1/repos/{o}/{r}/actions/runs[?workflow_file=&status=&event=&head_ref=&actor=&page=&per_page=]
//	GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}
//	GET /api/v1/repos/{o}/{r}/actions/runs/{run_id}/jobs
//
// Mirrors GitHub's \`/repos/{o}/{r}/actions/runs\` family.
// Scope: \`repo:read\`. Reuses the rich internal/actions sqlc
// surface, so filters cover every shape the HTML "Actions" tab
// already supports.
//
// Lifecycle controls (cancel, rerun, approve) stay on the
// existing actions-lifecycle routes — this surface is read-only.
func (h *Handlers) mountActionsRuns(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/actions/runs", h.actionsRunsList)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/runs/{run_id}", h.actionsRunGet)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/runs/{run_id}/jobs", h.actionsRunJobs)
	})
}

type actionsRunResponse struct {
	ID            int64  `json:"id"`
	RunNumber     int64  `json:"run_number"`
	WorkflowFile  string `json:"workflow_file"`
	WorkflowName  string `json:"workflow_name,omitempty"`
	HeadSHA       string `json:"head_sha"`
	HeadRef       string `json:"head_ref,omitempty"`
	Event         string `json:"event"`
	Status        string `json:"status"`
	Conclusion    string `json:"conclusion,omitempty"`
	ActorID       int64  `json:"actor_id,omitempty"`
	ActorUsername string `json:"actor_username,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	CompletedAt   string `json:"completed_at,omitempty"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type actionsJobResponse struct {
	ID              int64    `json:"id"`
	RunID           int64    `json:"run_id"`
	JobIndex        int32    `json:"job_index"`
	JobKey          string   `json:"job_key"`
	JobName         string   `json:"job_name,omitempty"`
	RunsOn          string   `json:"runs_on,omitempty"`
	Status          string   `json:"status"`
	Conclusion      string   `json:"conclusion,omitempty"`
	CancelRequested bool     `json:"cancel_requested"`
	NeedsJobs       []string `json:"needs_jobs,omitempty"`
	StartedAt       string   `json:"started_at,omitempty"`
	CompletedAt     string   `json:"completed_at,omitempty"`
	CreatedAt       string   `json:"created_at"`
}

func presentActionsRun(row actionsdb.ListWorkflowRunsForRepoRow) actionsRunResponse {
	out := actionsRunResponse{
		ID:            row.ID,
		RunNumber:     row.RunIndex,
		WorkflowFile:  row.WorkflowFile,
		WorkflowName:  row.WorkflowName,
		HeadSHA:       row.HeadSha,
		HeadRef:       row.HeadRef,
		Event:         string(row.Event),
		Status:        string(row.Status),
		ActorUsername: row.ActorUsername,
		CreatedAt:     row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:     row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if row.Conclusion.Valid {
		out.Conclusion = string(row.Conclusion.CheckConclusion)
	}
	if row.ActorUserID.Valid {
		out.ActorID = row.ActorUserID.Int64
	}
	if row.StartedAt.Valid {
		out.StartedAt = row.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	if row.CompletedAt.Valid {
		out.CompletedAt = row.CompletedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

// presentActionsRunFromBare adapts the single-run sqlc row (which
// lacks the joined actor_username column) to the same response shape
// as the list endpoint.
func presentActionsRunFromBare(row actionsdb.WorkflowRun) actionsRunResponse {
	out := actionsRunResponse{
		ID:           row.ID,
		RunNumber:    row.RunIndex,
		WorkflowFile: row.WorkflowFile,
		WorkflowName: row.WorkflowName,
		HeadSHA:      row.HeadSha,
		HeadRef:      row.HeadRef,
		Event:        string(row.Event),
		Status:       string(row.Status),
		CreatedAt:    row.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt:    row.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
	if row.Conclusion.Valid {
		out.Conclusion = string(row.Conclusion.CheckConclusion)
	}
	if row.ActorUserID.Valid {
		out.ActorID = row.ActorUserID.Int64
	}
	if row.StartedAt.Valid {
		out.StartedAt = row.StartedAt.Time.UTC().Format(time.RFC3339)
	}
	if row.CompletedAt.Valid {
		out.CompletedAt = row.CompletedAt.Time.UTC().Format(time.RFC3339)
	}
	return out
}

func (h *Handlers) actionsRunsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := r.URL.Query()
	params := actionsdb.ListWorkflowRunsForRepoParams{
		RepoID:     repo.ID,
		PageLimit:  int32(perPage),
		PageOffset: int32((page - 1) * perPage),
	}
	if v := strings.TrimSpace(q.Get("workflow_file")); v != "" {
		params.WorkflowFile = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(q.Get("head_ref")); v != "" {
		params.HeadRef = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(q.Get("actor")); v != "" {
		params.ActorUsername = pgtype.Text{String: v, Valid: true}
	}
	if v := strings.TrimSpace(q.Get("event")); v != "" {
		params.Event = actionsdb.NullWorkflowRunEvent{
			WorkflowRunEvent: actionsdb.WorkflowRunEvent(v), Valid: true,
		}
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		params.Status = actionsdb.NullWorkflowRunStatus{
			WorkflowRunStatus: actionsdb.WorkflowRunStatus(v), Valid: true,
		}
	}
	if v := strings.TrimSpace(q.Get("conclusion")); v != "" {
		params.Conclusion = actionsdb.NullCheckConclusion{
			CheckConclusion: actionsdb.CheckConclusion(v), Valid: true,
		}
	}

	rq := actionsdb.New()
	total, err := rq.CountWorkflowRunsForRepo(r.Context(), h.d.Pool, actionsdb.CountWorkflowRunsForRepoParams{
		RepoID:        params.RepoID,
		WorkflowFile:  params.WorkflowFile,
		HeadRef:       params.HeadRef,
		Event:         params.Event,
		Status:        params.Status,
		Conclusion:    params.Conclusion,
		ActorUsername: params.ActorUsername,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count workflow runs", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := rq.ListWorkflowRunsForRepo(r.Context(), h.d.Pool, params)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list workflow runs", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]actionsRunResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentActionsRun(row))
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) actionsRunGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "run_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	row, err := actionsdb.New().GetWorkflowRunByID(r.Context(), h.d.Pool, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "run not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get workflow run", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	// Cross-repo probes return 404 so a caller can't enumerate run
	// ids globally via the response status.
	if row.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	writeJSON(w, http.StatusOK, presentActionsRunFromBare(row))
}

func (h *Handlers) actionsRunJobs(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "run_id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	// Re-resolve the run so we can apply the cross-repo guard before
	// touching the jobs table.
	run, err := actionsdb.New().GetWorkflowRunByID(r.Context(), h.d.Pool, id)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	if run.RepoID != repo.ID {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	rows, err := actionsdb.New().ListJobsForRun(r.Context(), h.d.Pool, id)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list jobs", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]actionsJobResponse, 0, len(rows))
	for _, row := range rows {
		job := actionsJobResponse{
			ID:              row.ID,
			RunID:           row.RunID,
			JobIndex:        row.JobIndex,
			JobKey:          row.JobKey,
			JobName:         row.JobName,
			RunsOn:          row.RunsOn,
			Status:          string(row.Status),
			CancelRequested: row.CancelRequested,
			NeedsJobs:       append([]string{}, row.NeedsJobs...),
			CreatedAt:       row.CreatedAt.Time.UTC().Format(time.RFC3339),
		}
		if row.Conclusion.Valid {
			job.Conclusion = string(row.Conclusion.CheckConclusion)
		}
		if row.StartedAt.Valid {
			job.StartedAt = row.StartedAt.Time.UTC().Format(time.RFC3339)
		}
		if row.CompletedAt.Valid {
			job.CompletedAt = row.CompletedAt.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, job)
	}
	writeJSON(w, http.StatusOK, out)
}
