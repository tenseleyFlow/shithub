// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsLifecycle registers user/PAT-authenticated Actions lifecycle
// controls. Runner-owned job endpoints stay in runners.go and use job JWTs.
func (h *Handlers) mountActionsLifecycle(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/jobs/{id}/cancel", h.workflowJobCancel)
		r.Post("/api/v1/runs/{id}/rerun", h.workflowRunRerun)
	})
}

func (h *Handlers) workflowJobCancel(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	jobID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || jobID <= 0 {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return
	}
	job, run, repo, ok := h.resolveCancellableJob(w, r, auth.PolicyActor(), jobID)
	if !ok {
		return
	}
	result, err := actionslifecycle.CancelJob(r.Context(), actionslifecycle.Deps{
		Pool:   h.d.Pool,
		Logger: h.d.Logger,
	}, job.ID, actionslifecycle.CancelReasonUser)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "api actions cancel job", "job_id", job.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "cancel failed")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"job_id":         job.ID,
		"run_id":         run.ID,
		"repo_id":        repo.ID,
		"changed_jobs":   len(result.ChangedJobs),
		"run_completed":  result.RunCompleted,
		"run_conclusion": string(result.RunConclusion),
	})
}

func (h *Handlers) resolveCancellableJob(
	w http.ResponseWriter,
	r *http.Request,
	actor policy.Actor,
	jobID int64,
) (actionsdb.WorkflowJob, actionsdb.WorkflowRun, reposdb.Repo, bool) {
	q := actionsdb.New()
	job, err := q.GetWorkflowJobByID(r.Context(), h.d.Pool, jobID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return actionsdb.WorkflowJob{}, actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	run, err := q.GetWorkflowRunByID(r.Context(), h.d.Pool, job.RunID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "job not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "run lookup failed")
		}
		return actionsdb.WorkflowJob{}, actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	repo, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, run.RepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "job not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "repo lookup failed")
		}
		return actionsdb.WorkflowJob{}, actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoWrite, policy.NewRepoRefFromRepo(repo)).Allow {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return actionsdb.WorkflowJob{}, actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	// PRO-EXT_SR2-10 (audit C2): job-id → run-id → repo-id chain
	// bypasses lookupRepoByLogin, so the PAT binding check is not
	// implicit. Verify the resolved repo is in the binding set.
	if !patBindingAllowsRepo(r, repo.ID) {
		writeAPIError(w, http.StatusNotFound, "job not found")
		return actionsdb.WorkflowJob{}, actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	return job, run, repo, true
}
