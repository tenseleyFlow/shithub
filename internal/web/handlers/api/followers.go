// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/social"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apipage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountFollowers registers the S50 §17 followers / following REST surface.
//
//	GET    /api/v1/users/{username}/followers                paginated followers list
//	GET    /api/v1/users/{username}/following                paginated following list
//	GET    /api/v1/user/following/{target_username}          204/404 membership probe
//	PUT    /api/v1/user/following/{target_username}          follow
//	DELETE /api/v1/user/following/{target_username}          unfollow
//
// Scopes: `user:read` on GETs, `user:write` on the mutations.
// Mirrors gh's contract. Cannot-follow-self → 422; self-follow
// probe always returns 404 to keep the response uniform.
func (h *Handlers) mountFollowers(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/users/{username}/followers", h.followersList)
		r.Get("/api/v1/users/{username}/following", h.followingList)
		r.Get("/api/v1/user/following/{target}", h.followingProbe)
	})
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserWrite))
		r.Put("/api/v1/user/following/{target}", h.followPut)
		r.Delete("/api/v1/user/following/{target}", h.followDelete)
	})
}

type followUserResponse struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	FollowedAt  string `json:"followed_at"`
}

func (h *Handlers) followersList(w http.ResponseWriter, r *http.Request) {
	user, ok := h.lookupUserOr404(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := socialdb.New()
	total, _ := q.CountFollowersForUser(r.Context(), h.d.Pool, pgtype.Int8{Int64: user.ID, Valid: true})
	rows, err := q.ListFollowersForUser(r.Context(), h.d.Pool, socialdb.ListFollowersForUserParams{
		FolloweeUserID: pgtype.Int8{Int64: user.ID, Valid: true},
		Limit:          int32(perPage),
		Offset:         int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list followers", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]followUserResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, followUserResponse{
			UserID:      row.UserID,
			Username:    row.Username,
			DisplayName: row.DisplayName,
			FollowedAt:  row.FollowedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) followingList(w http.ResponseWriter, r *http.Request) {
	user, ok := h.lookupUserOr404(w, r, chi.URLParam(r, "username"))
	if !ok {
		return
	}
	page, perPage := apipage.ParseQuery(r, apipage.DefaultPerPage, apipage.MaxPerPage)
	q := socialdb.New()
	total, _ := q.CountFollowingForUser(r.Context(), h.d.Pool, user.ID)
	rows, err := q.ListFollowingUsersForUser(r.Context(), h.d.Pool, socialdb.ListFollowingUsersForUserParams{
		FollowerUserID: user.ID,
		Limit:          int32(perPage),
		Offset:         int32((page - 1) * perPage),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list following", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]followUserResponse, 0, len(rows))
	for _, row := range rows {
		var id int64
		if row.UserID.Valid {
			id = row.UserID.Int64
		}
		out = append(out, followUserResponse{
			UserID:      id,
			Username:    row.Username,
			DisplayName: row.DisplayName,
			FollowedAt:  row.FollowedAt.Time.UTC().Format(time.RFC3339),
		})
	}
	link := apipage.Page{Current: page, PerPage: perPage, Total: int(total)}.LinkHeader(h.d.BaseURL, sanitizedURL(r))
	if link != "" {
		w.Header().Set("Link", link)
	}
	writeJSON(w, http.StatusOK, out)
}

// followingProbe: 204 if the authenticated user follows {target},
// 404 otherwise. Mirrors gh's `/user/following/{username}` shape.
func (h *Handlers) followingProbe(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	target, ok := h.lookupUserOr404(w, r, chi.URLParam(r, "target"))
	if !ok {
		return
	}
	following, err := social.IsFollowingUser(r.Context(), h.followerDeps(), auth.UserID, target.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: follow probe", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if !following {
		writeAPIError(w, http.StatusNotFound, "not following")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) followPut(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	target, ok := h.lookupUserOr404(w, r, chi.URLParam(r, "target"))
	if !ok {
		return
	}
	if err := social.FollowUser(r.Context(), h.followerDeps(), auth.UserID, target.ID); err != nil {
		writeFollowError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) followDelete(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	target, ok := h.lookupUserOr404(w, r, chi.URLParam(r, "target"))
	if !ok {
		return
	}
	if err := social.UnfollowUser(r.Context(), h.followerDeps(), auth.UserID, target.ID); err != nil {
		writeFollowError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// followerDeps builds a social.Deps populated with the throttle
// limiter so the orchestrator's per-user follow-rate guard works.
func (h *Handlers) followerDeps() social.Deps {
	lim := h.d.Throttle
	if lim == nil {
		// Test fixtures may not wire a Throttle; the orchestrator
		// no-ops the rate-limit step when Limiter is nil.
		lim = throttle.NewLimiter()
	}
	return social.Deps{Pool: h.d.Pool, Audit: h.d.Audit, Limiter: lim}
}

// lookupUserOr404 resolves a username path segment to a user row,
// emitting a uniform 404 envelope on miss so callers can't probe for
// username existence via the response shape.
func (h *Handlers) lookupUserOr404(w http.ResponseWriter, r *http.Request, name string) (usersdb.User, bool) {
	user, err := h.q.GetUserByUsername(r.Context(), h.d.Pool, name)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeAPIError(w, http.StatusNotFound, "user not found")
			return usersdb.User{}, false
		}
		h.d.Logger.ErrorContext(r.Context(), "api: lookup user", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "lookup failed")
		return usersdb.User{}, false
	}
	return user, true
}

func writeFollowError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, social.ErrNotLoggedIn):
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
	case errors.Is(err, social.ErrCannotFollowSelf):
		writeAPIError(w, http.StatusUnprocessableEntity, "cannot follow yourself")
	case errors.Is(err, social.ErrFollowRateLimit):
		writeAPIError(w, http.StatusTooManyRequests, "follow rate limit exceeded")
	default:
		writeAPIError(w, http.StatusInternalServerError, "internal error")
	}
}
