// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	"github.com/tenseleyFlow/shithub/internal/social"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountWatching registers the S50 §15 watching/subscriptions REST surface.
//
//	GET    /api/v1/repos/{o}/{r}/subscribers                  paginated list of watchers
//	GET    /api/v1/repos/{o}/{r}/subscription                 viewer's current watch level
//	PUT    /api/v1/repos/{o}/{r}/subscription                 set an explicit level
//	DELETE /api/v1/repos/{o}/{r}/subscription                 revert to implicit default
//
// Scope: `repo:read` on GETs, `user:write` on the mutations
// (matches our convention — watching is a user-scoped preference,
// not a repo mutation; the HTML surface gates via the same
// `ActionWatchSet` policy regardless).
//
// "subscription" is the GitHub-flavored noun for a watch. shithub's
// internal vocabulary is "watch"; we keep the gh names on the public
// URL so existing CLI ports keep working.
func (h *Handlers) mountWatching(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeRepoRead))
		r.Get("/api/v1/repos/{owner}/{repo}/subscribers", h.subscribersList)
		r.Get("/api/v1/repos/{owner}/{repo}/subscription", h.subscriptionGet)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Put("/api/v1/repos/{owner}/{repo}/subscription", h.subscriptionPut)
		r.Delete("/api/v1/repos/{owner}/{repo}/subscription", h.subscriptionDelete)
	})
}

type subscriberResponse struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	Level       string `json:"level"`
	UpdatedAt   string `json:"updated_at"`
}

type subscriptionResponse struct {
	Level    string `json:"level"`
	Explicit bool   `json:"explicit"`
}

func (h *Handlers) subscribersList(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := socialdb.New()
	total, err := q.CountWatchersForRepo(r.Context(), h.d.Pool, repo.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: count watchers", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	rows, err := q.ListWatchersForRepo(r.Context(), h.d.Pool, socialdb.ListWatchersForRepoParams{
		RepoID: repo.ID,
		Limit:  int32(perPage),
		Offset: int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list watchers", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]subscriberResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, subscriberResponse{
			UserID:      row.UserID,
			Username:    row.Username,
			DisplayName: row.DisplayName,
			Level:       string(row.Level),
			UpdatedAt:   row.UpdatedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) subscriptionGet(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionRepoRead)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	// `explicit` distinguishes the implicit `participating` default
	// (no row in `watches`) from an explicit user choice. The CLI can
	// surface this so a "default" sticker doesn't look like the user
	// opted in.
	row, err := socialdb.New().GetWatch(r.Context(), h.d.Pool, socialdb.GetWatchParams{
		UserID: auth.UserID, RepoID: repo.ID,
	})
	if err != nil {
		level, lerr := social.CurrentLevel(r.Context(), h.socialDeps(), auth.UserID, repo.ID)
		if lerr != nil {
			h.d.Logger.ErrorContext(r.Context(), "api: subscription get", "error", lerr)
			writeAPIError(w, http.StatusInternalServerError, "lookup failed")
			return
		}
		writeJSON(w, http.StatusOK, subscriptionResponse{
			Level: string(level), Explicit: false,
		})
		return
	}
	writeJSON(w, http.StatusOK, subscriptionResponse{
		Level: string(row.Level), Explicit: true,
	})
}

type subscriptionPutRequest struct {
	Level string `json:"level"`
}

func (h *Handlers) subscriptionPut(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionWatchSet)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var body subscriptionPutRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := social.SetWatch(r.Context(), h.socialDeps(), auth.UserID, repo.ID, social.WatchLevel(body.Level)); err != nil {
		writeWatchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, subscriptionResponse{
		Level: body.Level, Explicit: true,
	})
}

func (h *Handlers) subscriptionDelete(w http.ResponseWriter, r *http.Request) {
	repo, ok := h.resolveAPIRepo(w, r, policy.ActionWatchSet)
	if !ok {
		return
	}
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	if err := social.UnsetWatch(r.Context(), h.socialDeps(), auth.UserID, repo.ID); err != nil {
		writeWatchError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// socialDeps materialises a social.Deps from the handler's Deps. We
// don't have a Logger field on social.Deps, just Pool + Audit.
func (h *Handlers) socialDeps() social.Deps {
	return social.Deps{Pool: h.d.Pool, Audit: h.d.Audit}
}

func writeWatchError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, social.ErrNotLoggedIn):
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, social.ErrInvalidWatchLevel):
		writeAPIError(w, http.StatusUnprocessableEntity, "level must be all, participating, or ignore")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
