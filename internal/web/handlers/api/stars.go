// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/social"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountStars registers the S26 PAT-authenticated star endpoints.
//
//	PUT    /api/v1/user/starred/{owner}/{repo} — idempotent star
//	DELETE /api/v1/user/starred/{owner}/{repo} — idempotent unstar
//	GET    /api/v1/user/starred                 — list current user's stars
//
// Scopes: user:write for star/unstar, user:read for list. Same shape
// as the existing /api/v1/user route (which uses user:read).
func (h *Handlers) mountStars(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Put("/api/v1/user/starred/{owner}/{repo}", h.starPut)
		r.Delete("/api/v1/user/starred/{owner}/{repo}", h.starDelete)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/starred", h.starsList)
	})
}

func (h *Handlers) starPut(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveStarTargetRepo(w, r)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := social.Star(r.Context(), social.Deps{Pool: h.d.Pool}, auth.UserID, repo.ID, policy.NewRepoRefFromRepo(repo).IsPublic()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "star failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) starDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveStarTargetRepo(w, r)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if err := social.Unstar(r.Context(), social.Deps{Pool: h.d.Pool}, auth.UserID, repo.ID, policy.NewRepoRefFromRepo(repo).IsPublic()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "unstar failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// starsList paginates the caller's starred repos.
func (h *Handlers) starsList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const pageSize = 50
	rows, err := socialdb.New().ListStarsForUser(r.Context(), h.d.Pool, socialdb.ListStarsForUserParams{
		UserID: auth.UserID, Limit: pageSize, Offset: int32((page - 1) * pageSize),
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, s := range rows {
		out = append(out, map[string]any{
			"repo_id":          s.RepoID,
			"name":             s.RepoName,
			"description":      s.Description,
			"visibility":       string(s.Visibility),
			"star_count":       s.StarCount,
			"primary_language": pgTextString(s.PrimaryLanguage),
			"starred_at":       s.StarredAt.Time.Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"page": page, "starred": out})
}

// resolveStarTargetRepo resolves /{owner}/{repo} into a repo row,
// gates by policy.ActionStarCreate (which checks visibility +
// suspension + login), and returns 404-shaped denials so private-
// repo existence isn't leaked.
func (h *Handlers) resolveStarTargetRepo(w http.ResponseWriter, r *http.Request) (reposdb.Repo, bool) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return reposdb.Repo{}, false
	}
	owner, err := usersdb.New().GetUserByUsername(r.Context(), h.d.Pool, chi.URLParam(r, "owner"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return reposdb.Repo{}, false
	}
	repo, err := reposdb.New().GetRepoByOwnerUserAndName(r.Context(), h.d.Pool, reposdb.GetRepoByOwnerUserAndNameParams{
		OwnerUserID: pgtype.Int8{Int64: owner.ID, Valid: true},
		Name:        chi.URLParam(r, "repo"),
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return reposdb.Repo{}, false
	}
	if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionStarCreate, policy.NewRepoRefFromRepo(repo)).Allow {
		writeAPIError(w, http.StatusNotFound, "repo not found")
		return reposdb.Repo{}, false
	}
	return repo, true
}

// pgTextString unwraps a nullable text column for JSON output. nil
// when invalid so encoding/json emits `null`.
func pgTextString(t pgtype.Text) any {
	if !t.Valid {
		return nil
	}
	return t.String
}
