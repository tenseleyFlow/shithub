// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/variables"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountActionsVariables registers the S50 §13 part 3 actions
// variables REST surface (repo + org). Variables are plaintext —
// `${{ vars.NAME }}` substitution in workflows — and unlike secrets
// have no encryption wrapper.
//
//	GET    /api/v1/repos/{o}/{r}/actions/variables
//	POST   /api/v1/repos/{o}/{r}/actions/variables
//	GET    /api/v1/repos/{o}/{r}/actions/variables/{name}
//	PATCH  /api/v1/repos/{o}/{r}/actions/variables/{name}
//	DELETE /api/v1/repos/{o}/{r}/actions/variables/{name}
//	GET    /api/v1/orgs/{org}/actions/variables
//	POST   /api/v1/orgs/{org}/actions/variables
//	GET    /api/v1/orgs/{org}/actions/variables/{name}
//	PATCH  /api/v1/orgs/{org}/actions/variables/{name}
//	DELETE /api/v1/orgs/{org}/actions/variables/{name}
//
// Scopes: `repo:read` on GETs, `repo:write` on mutations.
func (h *Handlers) mountActionsVariables(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/actions/variables", h.actionsVariablesListRepo)
		r.Get("/api/v1/repos/{owner}/{repo}/actions/variables/{name}", h.actionsVariablesGetRepo)
		r.Get("/api/v1/orgs/{org}/actions/variables", h.actionsVariablesListOrg)
		r.Get("/api/v1/orgs/{org}/actions/variables/{name}", h.actionsVariablesGetOrg)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/actions/variables", h.actionsVariablesCreateRepo)
		r.Patch("/api/v1/repos/{owner}/{repo}/actions/variables/{name}", h.actionsVariablesUpdateRepo)
		r.Delete("/api/v1/repos/{owner}/{repo}/actions/variables/{name}", h.actionsVariablesDeleteRepo)
		r.Post("/api/v1/orgs/{org}/actions/variables", h.actionsVariablesCreateOrg)
		r.Patch("/api/v1/orgs/{org}/actions/variables/{name}", h.actionsVariablesUpdateOrg)
		r.Delete("/api/v1/orgs/{org}/actions/variables/{name}", h.actionsVariablesDeleteOrg)
	})
}

type variableResponse struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type variableCreateRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type variableUpdateRequest struct {
	Value string `json:"value"`
}

func presentVariable(v variables.Variable) variableResponse {
	return variableResponse{
		Name:      v.Name,
		Value:     v.Value,
		CreatedAt: v.CreatedAt.Time.UTC().Format(time.RFC3339),
		UpdatedAt: v.UpdatedAt.Time.UTC().Format(time.RFC3339),
	}
}

// ─── repo scope ─────────────────────────────────────────────────────

func (h *Handlers) actionsVariablesListRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	rows, err := h.variablesDeps().List(r.Context(), variables.RepoScope(repo.ID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list repo variables", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]variableResponse, 0, len(rows))
	for _, v := range rows {
		out = append(out, presentVariable(v))
	}
	// S62 audit B13.
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "variables": out})
}

func (h *Handlers) actionsVariablesGetRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	v, err := h.variablesDeps().Get(r.Context(), variables.RepoScope(repo.ID), chi.URLParam(r, "name"))
	if err != nil {
		writeVariablesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, presentVariable(v))
}

func (h *Handlers) actionsVariablesCreateRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	var body variableCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.variablesDeps().Set(r.Context(), variables.RepoScope(repo.ID), body.Name, body.Value, auth.UserID); err != nil {
		writeVariablesError(w, err)
		return
	}
	v, err := h.variablesDeps().Get(r.Context(), variables.RepoScope(repo.ID), body.Name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusCreated, presentVariable(v))
}

func (h *Handlers) actionsVariablesUpdateRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	var body variableUpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.variablesDeps().Set(r.Context(), variables.RepoScope(repo.ID), chi.URLParam(r, "name"), body.Value, auth.UserID); err != nil {
		writeVariablesError(w, err)
		return
	}
	v, err := h.variablesDeps().Get(r.Context(), variables.RepoScope(repo.ID), chi.URLParam(r, "name"))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusOK, presentVariable(v))
}

func (h *Handlers) actionsVariablesDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoWrite)
	if !ok {
		return
	}
	if err := h.variablesDeps().Delete(r.Context(), variables.RepoScope(repo.ID), chi.URLParam(r, "name")); err != nil {
		writeVariablesError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── org scope ──────────────────────────────────────────────────────

func (h *Handlers) actionsVariablesListOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	rows, err := h.variablesDeps().List(r.Context(), variables.OrgScope(org.ID))
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list org variables", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]variableResponse, 0, len(rows))
	for _, v := range rows {
		out = append(out, presentVariable(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{"total_count": len(out), "variables": out})
}

func (h *Handlers) actionsVariablesGetOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	v, err := h.variablesDeps().Get(r.Context(), variables.OrgScope(org.ID), chi.URLParam(r, "name"))
	if err != nil {
		writeVariablesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, presentVariable(v))
}

func (h *Handlers) actionsVariablesCreateOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	if !h.requireOrgFeature(w, r, org, entitlements.FeatureOrgActionsVariables, "Organization Actions variables") {
		return
	}
	var body variableCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.variablesDeps().Set(r.Context(), variables.OrgScope(org.ID), body.Name, body.Value, auth.UserID); err != nil {
		writeVariablesError(w, err)
		return
	}
	v, err := h.variablesDeps().Get(r.Context(), variables.OrgScope(org.ID), body.Name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusCreated, presentVariable(v))
}

func (h *Handlers) actionsVariablesUpdateOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	if !h.requireOrgFeature(w, r, org, entitlements.FeatureOrgActionsVariables, "Organization Actions variables") {
		return
	}
	var body variableUpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32*1024)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := h.variablesDeps().Set(r.Context(), variables.OrgScope(org.ID), chi.URLParam(r, "name"), body.Value, auth.UserID); err != nil {
		writeVariablesError(w, err)
		return
	}
	v, err := h.variablesDeps().Get(r.Context(), variables.OrgScope(org.ID), chi.URLParam(r, "name"))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload failed")
		return
	}
	writeJSON(w, http.StatusOK, presentVariable(v))
}

func (h *Handlers) actionsVariablesDeleteOrg(w http.ResponseWriter, r *http.Request) {
	org, ok := h.resolveAPIOrg(w, r, chi.URLParam(r, "org"))
	if !ok {
		return
	}
	if !h.requireAPIOrgOwner(w, r, org) {
		return
	}
	if err := h.variablesDeps().Delete(r.Context(), variables.OrgScope(org.ID), chi.URLParam(r, "name")); err != nil {
		writeVariablesError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── helpers ────────────────────────────────────────────────────────

func (h *Handlers) variablesDeps() variables.Deps {
	return variables.Deps{Pool: h.d.Pool}
}

func writeVariablesError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, variables.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "variable not found")
	case errors.Is(err, variables.ErrInvalidName):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, variables.ErrValueTooLong):
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
