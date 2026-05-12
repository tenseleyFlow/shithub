// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	policydb "github.com/tenseleyFlow/shithub/internal/auth/policy/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountCollaborators registers the S50 §10 collaborators REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/collaborators                       list
//	GET    /api/v1/repos/{o}/{r}/collaborators/{username}            membership probe (204 / 404)
//	GET    /api/v1/repos/{o}/{r}/collaborators/{username}/permission permission level
//	PUT    /api/v1/repos/{o}/{r}/collaborators/{username}            add or upgrade
//	DELETE /api/v1/repos/{o}/{r}/collaborators/{username}            remove
//
// Scope: `repo:read` on GETs, `repo:write` on mutations. Policy gate
// for writes is `ActionRepoAdmin` — only repo owners + admin
// collaborators can grant / revoke (we layer the role check on top of
// the broader scope, since shithub PATs don't currently mint a
// separate admin-only scope). Owner ownership is surfaced from the
// `repo` row, not the collaborator table.
func (h *Handlers) mountCollaborators(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/collaborators", h.collaboratorsList)
		r.Get("/api/v1/repos/{owner}/{repo}/collaborators/{username}", h.collaboratorMembership)
		r.Get("/api/v1/repos/{owner}/{repo}/collaborators/{username}/permission", h.collaboratorPermission)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoWrite))
		r.Put("/api/v1/repos/{owner}/{repo}/collaborators/{username}", h.collaboratorPut)
		r.Delete("/api/v1/repos/{owner}/{repo}/collaborators/{username}", h.collaboratorDelete)
	})
}

type collaboratorResponse struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
}

func (h *Handlers) collaboratorsList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	rows, err := policydb.New().ListCollabs(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list collabs", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]collaboratorResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, collaboratorResponse{
			UserID:      c.UserID,
			Username:    c.Username,
			DisplayName: c.DisplayName,
			Role:        string(c.Role),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// collaboratorMembership mirrors GitHub's GET .../{username} which
// returns 204 if the user is a collaborator (any role) and 404
// otherwise. We don't include a body — clients use the 204 vs 404
// status code directly.
func (h *Handlers) collaboratorMembership(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	user, ok := h.lookupCollabUser(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	_, err := policydb.New().GetCollabRole(r.Context(), h.d.Pool, policydb.GetCollabRoleParams{
		RepoID: repo.ID, UserID: user.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "not a collaborator")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "api: get collab role", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) collaboratorPermission(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	user, ok := h.lookupCollabUser(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	role, err := policydb.New().GetCollabRole(r.Context(), h.d.Pool, policydb.GetCollabRoleParams{
		RepoID: repo.ID, UserID: user.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{
				"user":       user.Username,
				"permission": "none",
			})
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":       user.Username,
		"permission": string(role),
	})
}

type collaboratorPutRequest struct {
	Role string `json:"role"`
}

func (h *Handlers) collaboratorPut(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	user, ok := h.lookupCollabUser(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	// Refuse to enroll the owner as a collaborator — they already
	// implicitly hold every permission, so a row would be confusing
	// at best (and could lock the legitimate owner into a downgraded
	// role at worst).
	if repo.OwnerUserID.Valid && repo.OwnerUserID.Int64 == user.ID {
		writeAPIError(w, http.StatusUnprocessableEntity, "owner already has full access")
		return
	}
	var body collaboratorPutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	role, err := parseCollabRole(body.Role)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err := policydb.New().UpsertCollabRole(r.Context(), h.d.Pool, policydb.UpsertCollabRoleParams{
		RepoID: repo.ID,
		UserID: user.ID,
		Role:   role,
		AddedByUserID: pgtype.Int8{
			Int64: auth.UserID,
			Valid: auth.UserID != 0,
		},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: upsert collab", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "upsert failed")
		return
	}
	writeJSON(w, http.StatusOK, collaboratorResponse{
		UserID:      user.ID,
		Username:    user.Username,
		DisplayName: user.DisplayName,
		Role:        string(role),
	})
}

func (h *Handlers) collaboratorDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoAdmin)
	if !ok {
		return
	}
	user, ok := h.lookupCollabUser(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	if err := policydb.New().RemoveCollab(r.Context(), h.d.Pool, policydb.RemoveCollabParams{
		RepoID: repo.ID, UserID: user.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: remove collab", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "remove failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lookupCollabUser resolves the username path segment to a user row.
// Returns 404 (with no existence-leak) if the username doesn't match.
func (h *Handlers) lookupCollabUser(w http.ResponseWriter, r *http.Request, raw string) (usersdb.User, bool) {
	name := strings.TrimSpace(raw)
	if name == "" {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return usersdb.User{}, false
	}
	user, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, name)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "user not found")
		return usersdb.User{}, false
	}
	return user, true
}

// parseCollabRole maps the JSON-body role string onto the schema enum.
// GitHub accepts a `permission` field with values pull / triage / push /
// maintain / admin; we accept both the gh-style names and our internal
// names. Returns 422-friendly errors.
func parseCollabRole(raw string) (policydb.CollabRole, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "read", "pull":
		return policydb.CollabRoleRead, nil
	case "triage":
		return policydb.CollabRoleTriage, nil
	case "write", "push":
		return policydb.CollabRoleWrite, nil
	case "maintain":
		return policydb.CollabRoleMaintain, nil
	case "admin":
		return policydb.CollabRoleAdmin, nil
	}
	return "", errors.New("role must be read|triage|write|maintain|admin (or gh: pull/push)")
}
