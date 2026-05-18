// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	actionslifecycle "github.com/tenseleyFlow/shithub/internal/actions/lifecycle"
	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

func (h *Handlers) repoActionRunApprove(w http.ResponseWriter, r *http.Request) {
	h.repoActionRunApprovalDecision(w, r, true)
}

func (h *Handlers) repoActionRunReject(w http.ResponseWriter, r *http.Request) {
	h.repoActionRunApprovalDecision(w, r, false)
}

func (h *Handlers) repoActionRunApprovalDecision(w http.ResponseWriter, r *http.Request, approve bool) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionActionsApprove)
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
			h.d.Logger.WarnContext(r.Context(), "repo actions: lookup run for approval", "repo_id", row.ID, "run_index", runIndex, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		}
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	var action audit.Action
	if approve {
		if _, err := actionslifecycle.ApproveRun(r.Context(), actionslifecycle.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, run.ID, viewer.ID); err != nil {
			h.writeRepoApprovalError(w, r, run.ID, err)
			return
		}
		action = audit.ActionWorkflowRunApproved
		_ = worker.Notify(r.Context(), h.d.Pool)
	} else {
		if _, err := actionslifecycle.RejectRun(r.Context(), actionslifecycle.Deps{Pool: h.d.Pool, Logger: h.d.Logger}, run.ID, viewer.ID); err != nil {
			h.writeRepoApprovalError(w, r, run.ID, err)
			return
		}
		action = audit.ActionWorkflowRunRejected
	}
	actor, meta := viewer.AuditActor(map[string]any{
		"run_id":        run.ID,
		"run_index":     run.RunIndex,
		"workflow_file": run.WorkflowFile,
		"event":         string(run.Event),
		"head_ref":      run.HeadRef,
		"head_sha":      run.HeadSha,
	})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, action, audit.TargetRepo, row.ID, meta)
	http.Redirect(w, r, repoActionRunHref(owner.Username, row.Name, runIndex), http.StatusSeeOther)
}

func (h *Handlers) writeRepoApprovalError(w http.ResponseWriter, r *http.Request, runID int64, err error) {
	switch {
	case errors.Is(err, actionslifecycle.ErrRunNotApprovalPending):
		h.d.Render.HTTPError(w, r, http.StatusConflict, "run is not pending approval")
	case errors.Is(err, actionslifecycle.ErrApprovalActorRequired):
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "approval actor required")
	case errors.Is(err, actionslifecycle.ErrApprovalSelfReviewBlocked):
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "environment prevents self-review")
	default:
		h.d.Logger.WarnContext(r.Context(), "repo actions: approval decision", "run_id", runID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	}
}
