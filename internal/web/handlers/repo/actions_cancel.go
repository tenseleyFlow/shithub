// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) repoActionRunCancel(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	runIndex, ok := parsePositiveInt64Param(r, "runIndex")
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	run, err := actionsdb.New().GetWorkflowRunForRepoByIndex(r.Context(), h.d.Pool, actionsdb.GetWorkflowRunForRepoByIndexParams{
		RepoID:   row.ID,
		RunIndex: runIndex,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: lookup run for cancel", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	if _, err := actionslifecycle.CancelRun(r.Context(), actionslifecycle.Deps{
		Pool:   h.d.Pool,
		Logger: h.d.Logger,
	}, run.ID, actionslifecycle.CancelReasonUser); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: cancel run", "run_id", run.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, repoActionRunHref(owner.Username, row.Name, runIndex), http.StatusSeeOther)
}

func (h *Handlers) repoActionJobCancel(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoWrite)
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
	q := actionsdb.New()
	run, err := q.GetWorkflowRunForRepoByIndex(r.Context(), h.d.Pool, actionsdb.GetWorkflowRunForRepoByIndexParams{
		RepoID:   row.ID,
		RunIndex: runIndex,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		} else {
			h.d.Logger.WarnContext(r.Context(), "repo actions: lookup run for job cancel", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	jobs, err := q.ListJobsForRun(r.Context(), h.d.Pool, run.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: list jobs for cancel", "run_id", run.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	var jobID int64
	for _, job := range jobs {
		if job.JobIndex == jobIndex {
			jobID = job.ID
			break
		}
	}
	if jobID == 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if _, err := actionslifecycle.CancelJob(r.Context(), actionslifecycle.Deps{
		Pool:   h.d.Pool,
		Logger: h.d.Logger,
	}, jobID, actionslifecycle.CancelReasonUser); err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo actions: cancel job", "job_id", jobID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, repoActionRunHref(owner.Username, row.Name, runIndex)+"#job-"+strconv.FormatInt(int64(jobIndex), 10), http.StatusSeeOther)
}

func (h *Handlers) applyActionsCancelControls(r *http.Request, row reposdb.Repo, view *actionsRunDetailView) {
	if view == nil || view.IsTerminal {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	dec := policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, viewer.PolicyActor(), policy.ActionRepoWrite, policy.NewRepoRefFromRepo(row))
	if !dec.Allow {
		return
	}
	for i := range view.Jobs {
		if view.Jobs[i].IsCancellable && !view.Jobs[i].CancelRequested {
			view.Jobs[i].CanCancel = true
			view.CanCancel = true
		}
	}
}

func repoActionRunHref(owner, repoName string, runIndex int64) string {
	return "/" + owner + "/" + repoName + "/actions/runs/" + strconv.FormatInt(runIndex, 10)
}
