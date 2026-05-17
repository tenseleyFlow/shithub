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
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) workflowRunRerun(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	runID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || runID <= 0 {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return
	}
	run, repo, ok := h.resolveLifecycleRun(w, r, auth.PolicyActor(), runID)
	if !ok {
		return
	}
	result, err := actionslifecycle.RerunRun(r.Context(), actionslifecycle.Deps{
		Pool:   h.d.Pool,
		RepoFS: h.d.RepoFS,
		Logger: h.d.Logger,
	}, run.ID, auth.UserID)
	if err != nil {
		h.writeRerunError(w, r, run.ID, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"run_id":        result.RunID,
		"run_index":     result.RunIndex,
		"parent_run_id": result.ParentRunID,
		"repo_id":       repo.ID,
		"workflow_file": run.WorkflowFile,
		"head_sha":      run.HeadSha,
	})
}

func (h *Handlers) resolveLifecycleRun(
	w http.ResponseWriter,
	r *http.Request,
	actor policy.Actor,
	runID int64,
) (actionsdb.WorkflowRun, reposdb.Repo, bool) {
	q := actionsdb.New()
	run, err := q.GetWorkflowRunByID(r.Context(), h.d.Pool, runID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	repo, err := reposdb.New().GetRepoByID(r.Context(), h.d.Pool, run.RepoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "run not found")
		} else {
			writeAPIError(w, http.StatusInternalServerError, "repo lookup failed")
		}
		return actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, actor, policy.ActionRepoWrite, policy.NewRepoRefFromRepo(repo)).Allow {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	// PRO-EXT_SR2-10 (audit C2): the run lookup went via run id, not
	// owner/repo, so resolveLifecycleRun is one of the surfaces the
	// pat.go:30 contract calls out — the request PAT may be bound to
	// a different repo than the run's. Treat a binding mismatch as
	// 404 to preserve the "PAT can't probe other repos" property.
	if !patBindingAllowsRepo(r, repo.ID) {
		writeAPIError(w, http.StatusNotFound, "run not found")
		return actionsdb.WorkflowRun{}, reposdb.Repo{}, false
	}
	return run, repo, true
}

func (h *Handlers) writeRerunError(w http.ResponseWriter, r *http.Request, runID int64, err error) {
	switch {
	case errors.Is(err, actionslifecycle.ErrRunNotRerunnable):
		writeAPIError(w, http.StatusConflict, "run is not rerunnable")
	case errors.Is(err, actionslifecycle.ErrWorkflowSourceUnavailable):
		writeAPIError(w, http.StatusConflict, "workflow source unavailable")
	case errors.Is(err, actionslifecycle.ErrWorkflowSourceInvalid):
		writeAPIError(w, http.StatusUnprocessableEntity, "workflow source invalid")
	default:
		h.d.Logger.WarnContext(r.Context(), "api actions rerun", "run_id", runID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "rerun failed")
	}
}
