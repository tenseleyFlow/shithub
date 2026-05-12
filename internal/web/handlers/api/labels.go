// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/issues"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountLabels registers the S50 §3 repo-label REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/labels           list
//	POST   /api/v1/repos/{o}/{r}/labels           create
//	GET    /api/v1/repos/{o}/{r}/labels/{name}    fetch
//	PATCH  /api/v1/repos/{o}/{r}/labels/{name}    update
//	DELETE /api/v1/repos/{o}/{r}/labels/{name}    delete
//
// Labels are repo-scoped; manage requires ActionIssueLabel (the same
// gate the HTML labels page uses), so a triage-equivalent role is the
// minimum bar. List reads under ActionIssueRead.
func (h *Handlers) mountLabels(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/labels", h.labelsList)
		r.Get("/api/v1/repos/{owner}/{repo}/labels/{name}", h.labelGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Post("/api/v1/repos/{owner}/{repo}/labels", h.labelCreate)
		r.Patch("/api/v1/repos/{owner}/{repo}/labels/{name}", h.labelUpdate)
		r.Delete("/api/v1/repos/{owner}/{repo}/labels/{name}", h.labelDelete)
	})
}

type labelResponse struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func presentLabel(l issuesdb.Label) labelResponse {
	return labelResponse{
		ID:          l.ID,
		Name:        l.Name,
		Color:       l.Color,
		Description: l.Description,
		CreatedAt:   l.CreatedAt.Time.UTC().Format(time.RFC3339),
	}
}

func (h *Handlers) labelsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	rows, err := issuesdb.New().ListLabels(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list labels", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]labelResponse, 0, len(rows))
	for _, l := range rows {
		out = append(out, presentLabel(l))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) labelGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	row, err := issuesdb.New().GetLabelByName(r.Context(), h.d.Pool, issuesdb.GetLabelByNameParams{
		RepoID: repo.ID,
		Name:   chi.URLParam(r, "name"),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "label not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get label", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, presentLabel(row))
}

type labelCreateRequest struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func (h *Handlers) labelCreate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	var body labelCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	created, err := issues.CreateLabel(r.Context(), issues.Deps{
		Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit,
	}, issues.LabelCreateParams{
		RepoID:      repo.ID,
		Name:        body.Name,
		Color:       body.Color,
		Description: body.Description,
	})
	if err != nil {
		writeLabelsError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, presentLabel(created))
}

type labelUpdateRequest struct {
	Name        *string `json:"name,omitempty"`
	Color       *string `json:"color,omitempty"`
	Description *string `json:"description,omitempty"`
}

func (h *Handlers) labelUpdate(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	q := issuesdb.New()
	cur, err := q.GetLabelByName(r.Context(), h.d.Pool, issuesdb.GetLabelByNameParams{
		RepoID: repo.ID, Name: chi.URLParam(r, "name"),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "label not found")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get label", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	var body labelUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := cur.Name
	if body.Name != nil {
		name = *body.Name
	}
	color := cur.Color
	if body.Color != nil {
		color = *body.Color
	}
	desc := cur.Description
	if body.Description != nil {
		desc = *body.Description
	}
	if err := issues.UpdateLabel(r.Context(), issues.Deps{
		Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit,
	}, issues.LabelUpdateParams{
		ID: cur.ID, Name: name, Color: color, Description: desc,
	}); err != nil {
		writeLabelsError(w, err)
		return
	}
	fresh, _ := q.GetLabelByName(r.Context(), h.d.Pool, issuesdb.GetLabelByNameParams{
		RepoID: repo.ID, Name: name,
	})
	writeJSON(w, http.StatusOK, presentLabel(fresh))
}

func (h *Handlers) labelDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueLabel)
	if !ok {
		return
	}
	q := issuesdb.New()
	cur, err := q.GetLabelByName(r.Context(), h.d.Pool, issuesdb.GetLabelByNameParams{
		RepoID: repo.ID, Name: chi.URLParam(r, "name"),
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "label not found")
		return
	}
	if err := issues.DeleteLabel(r.Context(), issues.Deps{
		Pool: h.d.Pool, Logger: h.d.Logger, Audit: h.d.Audit,
	}, cur.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: delete label", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeLabelsError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, issues.ErrLabelExists):
		writeAPIError(w, http.StatusConflict, "label name already taken on this repo")
	case errors.Is(err, issues.ErrLabelInvalidColor):
		writeAPIError(w, http.StatusUnprocessableEntity, "color must be 6 hex chars")
	default:
		// CreateLabel returns plain errors for bad-name validation; map
		// generically as 422 since those are user-input failures.
		if err != nil && err.Error() != "" && (containsPrefix(err.Error(), "issues:")) {
			writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}

func containsPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
