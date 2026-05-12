// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountAssignees registers the assignee-suggestion endpoint. Mirrors
// GitHub's GET /repos/{o}/{r}/assignees — the set of users who can be
// assigned to issues / PRs on this repo.
//
// Scope: repo:read. Returns repo owner (user-owned) and direct
// collaborators. Org-owned repos: we surface direct collaborators
// only; org member expansion is a follow-up if the CLI needs it.
func (h *Handlers) mountAssignees(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/assignees", h.assigneesList)
	})
}

type assigneeResponse struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role,omitempty"`
}

func (h *Handlers) assigneesList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionIssueRead)
	if !ok {
		return
	}
	out := make([]assigneeResponse, 0, 4)
	seen := map[int64]struct{}{}
	if repo.OwnerUserID.Valid {
		u, err := h.q.GetUserByID(r.Context(), h.d.Pool, repo.OwnerUserID.Int64)
		if err == nil {
			out = append(out, assigneeResponse{
				UserID:      u.ID,
				Username:    u.Username,
				DisplayName: u.DisplayName,
				Role:        "owner",
			})
			seen[u.ID] = struct{}{}
		}
	}
	collabs, err := policydb.New().ListCollabs(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list collabs", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	for _, c := range collabs {
		if _, dup := seen[c.UserID]; dup {
			continue
		}
		out = append(out, assigneeResponse{
			UserID:      c.UserID,
			Username:    c.Username,
			DisplayName: c.DisplayName,
			Role:        string(c.Role),
		})
		seen[c.UserID] = struct{}{}
	}
	writeJSON(w, http.StatusOK, out)
}
