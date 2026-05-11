// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) contributionSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawName := chi.URLParam(r, "username")
	lower := strings.ToLower(rawName)
	if authpkg.IsReserved(lower) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}
	viewer := middleware.CurrentUserFromContext(ctx)
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "")
		return
	}

	user, err := h.q.GetUserByUsername(ctx, h.d.Pool, rawName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
			return
		}
		h.d.Logger.ErrorContext(ctx, "profile contributions: user lookup", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if user.SuspendedAt.Valid || user.DeletedAt.Valid {
		h.d.Render.HTTPError(w, r, http.StatusGone, "")
		return
	}
	if viewer.ID != user.ID {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "")
		return
	}

	includePrivate := r.PostFormValue("include_private_contributions") == "1"
	if err := h.q.UpdateUserPrivateContributions(ctx, h.d.Pool, usersdb.UpdateUserPrivateContributionsParams{
		ID:                          user.ID,
		IncludePrivateContributions: includePrivate,
	}); err != nil {
		h.d.Logger.ErrorContext(ctx, "profile contributions: update settings", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, profileContributionSettingsReturnPath(r.PostFormValue("return_to"), user.Username), http.StatusSeeOther)
}

func profileContributionSettingsReturnPath(raw, username string) string {
	fallback := "/" + url.PathEscape(username)
	u, err := url.Parse(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Path != fallback {
		return fallback
	}
	u.Scheme = ""
	u.Host = ""
	u.User = nil
	u.Fragment = ""
	return u.RequestURI()
}
