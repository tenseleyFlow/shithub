// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountStargazers registers the S50 §19 stargazers / starred-list REST
// surface. Distinct from the existing S26 stars routes — those are the
// caller-self read + write surface (`/api/v1/user/starred`,
// `/api/v1/user/starred/{owner}/{repo}`); §19 adds the public
// directories needed for parity with gh's `/repos/{o}/{r}/stargazers`
// and `/users/{u}/starred`.
//
//	GET /api/v1/repos/{owner}/{repo}/stargazers   paginated users who starred this repo
//	GET /api/v1/users/{username}/starred           paginated repos a user has starred
//
// Scopes: `repo:read` on the repo-side list, `user:read` on the user-side
// list. Cross-user `/users/{u}/starred` filters output to repos the
// caller can read so private stars don't leak via this surface.
func (h *Handlers) mountStargazers(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/stargazers", h.stargazersList)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/users/{username}/starred", h.userStarredList)
	})
}

type stargazerResponse struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	StarredAt   string `json:"starred_at"`
}

type starredRepoResponse struct {
	RepoID          int64  `json:"repo_id"`
	OwnerSlug       string `json:"owner"`
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Visibility      string `json:"visibility"`
	StarCount       int64  `json:"star_count"`
	PrimaryLanguage any    `json:"primary_language"`
	UpdatedAt       string `json:"updated_at"`
	StarredAt       string `json:"starred_at"`
}

// stargazersList paginates the users who have starred {owner}/{repo}.
// Reuses the existing socialdb.ListStargazersForRepo query.
func (h *Handlers) stargazersList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := socialdb.New()
	total, err := q.CountStargazersForRepo(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count stargazers", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListStargazersForRepo(r.Context(), h.d.Pool, socialdb.ListStargazersForRepoParams{
		RepoID: repo.ID,
		Limit:  int32(perPage),
		Offset: int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list stargazers", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]stargazerResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, stargazerResponse{
			UserID:      row.UserID,
			Username:    row.Username,
			DisplayName: row.DisplayName,
			StarredAt:   row.StarredAt.Time.UTC().Format(time.RFC3339),
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

// userStarredList paginates the repos {username} has starred. The DB
// query returns every star regardless of visibility; we post-filter
// against policy.ActionRepoRead so private repos a caller can't see
// don't leak via the user's profile feed. Total counts reflect the raw
// row count (matching gh's behavior — the user's star count is public
// even when individual repos are filtered).
func (h *Handlers) userStarredList(w http.ResponseWriter, r *http.Request) {
	user, ok := h.lookupUserOr404(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := socialdb.New()
	total, err := q.CountStarsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count user stars", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListStarsForUser(r.Context(), h.d.Pool, socialdb.ListStarsForUserParams{
		UserID: user.ID,
		Limit:  int32(perPage),
		Offset: int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user stars", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	out := make([]starredRepoResponse, 0, len(rows))
	for _, row := range rows {
		ref := policy.RepoRef{
			ID:          row.RepoID,
			Visibility:  string(row.Visibility),
			OwnerUserID: row.OwnerUserID.Int64,
			OwnerOrgID:  row.OwnerOrgID.Int64,
		}
		if !policy.Can(r.Context(), policy.Deps{Pool: h.d.Pool}, auth.PolicyActor(), policy.ActionRepoRead, ref).Allow {
			continue
		}
		out = append(out, starredRepoResponse{
			RepoID:          row.RepoID,
			OwnerSlug:       row.OwnerSlug,
			Name:            row.RepoName,
			Description:     row.Description,
			Visibility:      string(row.Visibility),
			StarCount:       row.StarCount,
			PrimaryLanguage: pgTextString(row.PrimaryLanguage),
			UpdatedAt:       row.UpdatedAt.Time.UTC().Format(time.RFC3339),
			StarredAt:       row.StarredAt.Time.UTC().Format(time.RFC3339),
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}
