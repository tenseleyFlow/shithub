// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"encoding/json"
	"errors"
	"io"
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
//	GET    /api/v1/repos/{o}/{r}/subscription                 viewer's current subscription (gh-compat)
//	PUT    /api/v1/repos/{o}/{r}/subscription                 set subscription (gh-compat body)
//	DELETE /api/v1/repos/{o}/{r}/subscription                 revert to implicit default
//
// Scope: `repo:read` on GETs, `user:write` on the mutations
// (matches our convention — watching is a user-scoped preference,
// not a repo mutation; the HTML surface gates via the same
// `ActionWatchSet` policy regardless).
//
// "subscription" is the GitHub-flavored noun for a watch. shithub's
// internal vocabulary is "watch"; we keep the gh names on the public
// URL so existing CLI ports keep working. The REST PUT body + GET
// response use the gh-compat `{subscribed, ignored}` pair (B5 audit
// decision): subscribed=true → level=all, ignored=true → level=ignore,
// both false → unset (back to implicit `participating`).
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

// subscriptionResponse is the gh-compat shape returned from
// GET/PUT /subscription. `reason` is always null for now — shithub
// doesn't surface why a subscription exists (no
// auto_subscription_reason field).
type subscriptionResponse struct {
	Subscribed    bool   `json:"subscribed"`
	Ignored       bool   `json:"ignored"`
	Reason        any    `json:"reason"`
	CreatedAt     string `json:"created_at"`
	URL           string `json:"url"`
	RepositoryURL string `json:"repository_url"`
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

// subscriptionGet returns the caller's explicit subscription in the
// gh-compat shape, or 404 if no explicit row exists (gh's contract —
// the implicit `participating` default has no REST representation).
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
	row, err := socialdb.New().GetWatch(r.Context(), h.d.Pool, socialdb.GetWatchParams{
		UserID: auth.UserID, RepoID: repo.ID,
	})
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, presentSubscription(row, chi.URLParam(r, "owner"), repo.Name))
}

// subscriptionPutRequest mirrors gh's PUT body. Both fields default to
// false when absent — meaning "remove explicit subscription".
type subscriptionPutRequest struct {
	Subscribed bool `json:"subscribed"`
	Ignored    bool `json:"ignored"`
}

// subscriptionPut maps the gh-compat body onto the server's internal
// WatchLevel:
//
//	subscribed=true, ignored=false → SetWatch(all)
//	subscribed=false, ignored=true → SetWatch(ignore)
//	subscribed=false, ignored=false → UnsetWatch  (204, back to default)
//	subscribed=true, ignored=true → 422
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
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Subscribed && body.Ignored {
		writeAPIError(w, http.StatusUnprocessableEntity, "subscribed and ignored cannot both be true")
		return
	}
	switch {
	case body.Subscribed:
		if err := social.SetWatch(r.Context(), h.socialDeps(), auth.UserID, repo.ID, social.WatchAll); err != nil {
			writeWatchError(w, err)
			return
		}
	case body.Ignored:
		if err := social.SetWatch(r.Context(), h.socialDeps(), auth.UserID, repo.ID, social.WatchIgnore); err != nil {
			writeWatchError(w, err)
			return
		}
	default:
		// Both false → clear explicit subscription.
		if err := social.UnsetWatch(r.Context(), h.socialDeps(), auth.UserID, repo.ID); err != nil {
			writeWatchError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	row, err := socialdb.New().GetWatch(r.Context(), h.d.Pool, socialdb.GetWatchParams{
		UserID: auth.UserID, RepoID: repo.ID,
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "reload subscription")
		return
	}
	writeJSON(w, http.StatusOK, presentSubscription(row, chi.URLParam(r, "owner"), repo.Name))
}

// presentSubscription maps the internal Watch row to the gh-compat
// response shape. ownerSlug + repoName echo the URL the client used so
// response bodies match the request path.
func presentSubscription(row socialdb.Watch, ownerSlug, repoName string) subscriptionResponse {
	resp := subscriptionResponse{
		Reason:        nil,
		CreatedAt:     row.UpdatedAt.Time.UTC().Format(time.RFC3339),
		URL:           "/api/v1/repos/" + ownerSlug + "/" + repoName + "/subscription",
		RepositoryURL: "/api/v1/repos/" + ownerSlug + "/" + repoName,
	}
	switch row.Level {
	case socialdb.WatchLevelAll:
		resp.Subscribed = true
	case socialdb.WatchLevelIgnore:
		resp.Ignored = true
	}
	return resp
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
