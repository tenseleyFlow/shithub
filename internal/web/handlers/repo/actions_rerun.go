// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) repoActionRunRerun(w http.ResponseWriter, r *http.Request) {
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
			h.d.Logger.WarnContext(r.Context(), "repo actions: lookup run for rerun", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	result, err := actionslifecycle.RerunRun(r.Context(), actionslifecycle.Deps{
		Pool:   h.d.Pool,
		RepoFS: h.d.RepoFS,
		Logger: h.d.Logger,
	}, run.ID, viewer.ID)
	if err != nil {
		h.writeRepoRerunError(w, r, run.ID, err)
		return
	}
	http.Redirect(w, r, repoActionRunHref(owner.Username, row.Name, result.RunIndex), http.StatusSeeOther)
}

func (h *Handlers) writeRepoRerunError(w http.ResponseWriter, r *http.Request, runID int64, err error) {
	switch {
	case errors.Is(err, actionslifecycle.ErrRunNotRerunnable):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "run is not rerunnable")
	case errors.Is(err, actionslifecycle.ErrWorkflowSourceUnavailable):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "workflow source unavailable")
	case errors.Is(err, actionslifecycle.ErrWorkflowSourceInvalid):
		h.d.Render.HTTPError(w, r, http.StatusUnprocessableEntity, "workflow source invalid")
	default:
		h.d.Logger.WarnContext(r.Context(), "repo actions: rerun", "run_id", runID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}
