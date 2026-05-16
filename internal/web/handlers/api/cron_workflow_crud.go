// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/cronworkflow"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-13c: CRUD REST for /api/v1/user/cron-dispatches/*.
// user:read on GETs, user:write on mutations.

func (h *Handlers) mountUserCronDispatches(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/cron-dispatches", h.userCronDispatchesList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Post("/api/v1/user/cron-dispatches", h.userCronDispatchesCreate)
		r.Delete("/api/v1/user/cron-dispatches/{id}", h.userCronDispatchesDelete)
		r.Post("/api/v1/user/cron-dispatches/{id}/disable", h.userCronDispatchesDisable)
	})
}

type cronDispatchResponse struct {
	ID             int64     `json:"id"`
	RepoID         int64     `json:"repo_id"`
	WorkflowFile   string    `json:"workflow_file"`
	Ref            string    `json:"ref"`
	CronExpr       string    `json:"cron_expr"`
	NextFireAt     time.Time `json:"next_fire_at"`
	LastFireAt     time.Time `json:"last_fire_at,omitempty"`
	LastFireStatus string    `json:"last_fire_status"`
	LastFireError  string    `json:"last_fire_error,omitempty"`
	Disabled       bool      `json:"disabled"`
}

type cronDispatchCreateRequest struct {
	RepoID       int64  `json:"repo_id"`
	WorkflowFile string `json:"workflow_file"`
	Ref          string `json:"ref"`
	CronExpr     string `json:"cron_expr"`
}

func presentCronDispatch(d cronworkflow.Dispatch) cronDispatchResponse {
	return cronDispatchResponse{
		ID:             d.ID,
		RepoID:         d.RepoID,
		WorkflowFile:   d.WorkflowFile,
		Ref:            d.Ref,
		CronExpr:       d.CronExpr,
		NextFireAt:     d.NextFireAt,
		LastFireAt:     d.LastFireAt,
		LastFireStatus: d.LastFireStatus,
		LastFireError:  d.LastFireError,
		Disabled:       d.Disabled,
	}
}

func (h *Handlers) userCronDispatchesList(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	rows, err := cronworkflow.Deps{Pool: h.d.Pool}.ListForUser(r.Context(), userID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list cron dispatches", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]cronDispatchResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, presentCronDispatch(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) userCronDispatchesCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	if !h.userCronWorkflowGate(r.Context(), w, userID, "create") {
		return
	}
	var body cronDispatchCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.RepoID == 0 {
		writeAPIError(w, http.StatusBadRequest, "repo_id is required")
		return
	}
	if !h.assertRepoOwnedByUser(r.Context(), w, body.RepoID, userID) {
		return
	}
	ref := body.Ref
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + strings.TrimPrefix(ref, "refs/heads/")
	}
	res, err := (cronworkflow.Deps{Pool: h.d.Pool}).Create(r.Context(), cronworkflow.CreateInput{
		UserID:       userID,
		RepoID:       body.RepoID,
		WorkflowFile: body.WorkflowFile,
		Ref:          ref,
		CronExpr:     body.CronExpr,
	})
	if err != nil {
		switch {
		case errors.Is(err, cronworkflow.ErrEmptyWorkflow):
			writeAPIError(w, http.StatusBadRequest, "workflow_file is required")
		case errors.Is(err, cronworkflow.ErrEmptyRef):
			writeAPIError(w, http.StatusBadRequest, "ref is required")
		case errors.Is(err, cronworkflow.ErrInvalidCronExpr):
			writeAPIError(w, http.StatusBadRequest, err.Error())
		default:
			h.d.Logger.ErrorContext(r.Context(), "api: create cron dispatch", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "create failed")
		}
		return
	}
	writeJSON(w, http.StatusCreated, presentCronDispatch(res))
}

func (h *Handlers) userCronDispatchesDelete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	id, ok := parseInt64Param(w, r, "id")
	if !ok {
		return
	}
	if !h.assertCronDispatchOwner(r.Context(), w, id, userID) {
		return
	}
	if err := (cronworkflow.Deps{Pool: h.d.Pool}).Delete(r.Context(), id); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete cron dispatch", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) userCronDispatchesDisable(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.requireUserPATAuth(w, r)
	if !ok {
		return
	}
	id, ok := parseInt64Param(w, r, "id")
	if !ok {
		return
	}
	if !h.assertCronDispatchOwner(r.Context(), w, id, userID) {
		return
	}
	if err := (cronworkflow.Deps{Pool: h.d.Pool}).Disable(r.Context(), id); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: disable cron dispatch", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "disable failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) assertCronDispatchOwner(ctx context.Context, w http.ResponseWriter, id, userID int64) bool {
	d, err := (cronworkflow.Deps{Pool: h.d.Pool}).GetByID(ctx, id)
	if err != nil || d.UserID != userID {
		writeAPIError(w, http.StatusNotFound, "not found")
		return false
	}
	return true
}

// assertRepoOwnedByUser is a cheap ownership check used only at
// create-time so a user can't schedule cron dispatches against repos
// they don't own. Returns true if repo's owner_user_id matches.
func (h *Handlers) assertRepoOwnedByUser(ctx context.Context, w http.ResponseWriter, repoID, userID int64) bool {
	var ownerUserID int64
	if err := h.d.Pool.QueryRow(
		ctx,
		`SELECT coalesce(owner_user_id, 0) FROM repos WHERE id = $1`, repoID,
	).Scan(&ownerUserID); err != nil || ownerUserID != userID {
		writeAPIError(w, http.StatusNotFound, "repo not found or not owned")
		return false
	}
	return true
}

// userCronWorkflowGate is the cron-CRUD gate. Same shape as the
// receiver/sweep gate; surface=cron-workflow-dispatch-rest.
func (h *Handlers) userCronWorkflowGate(ctx context.Context, w http.ResponseWriter, userID int64, action string) bool {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		orgbilling.PrincipalForUser(userID),
		entitlements.FeatureCronWorkflowDispatch)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "api: cron-workflow entitlement check", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "entitlement check failed")
		return false
	}
	if !decision.Allowed && h.d.Logger != nil {
		mode := "report_only"
		if h.d.BillingEnforce.UserCronWorkflowDispatch {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", orgbilling.PrincipalForUser(userID).String(),
			"principal_kind", string(orgbilling.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureCronWorkflowDispatch),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "cron-workflow-dispatch-rest",
			"action", action)
	}
	if !decision.Allowed && h.d.BillingEnforce.UserCronWorkflowDispatch {
		writeAPIError(w, http.StatusForbidden,
			"cron workflow dispatch requires a Pro subscription")
		return false
	}
	return true
}
