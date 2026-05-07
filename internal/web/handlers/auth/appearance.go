// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// allowedThemes mirrors the CHECK constraint on users.theme. Plus the
// empty string, which we treat the same as "auto" but persist as "" in
// the DB so the cookie wins on devices where the user hasn't set a
// preference.
var allowedThemes = map[string]struct{}{
	"":              {},
	"light":         {},
	"dark":          {},
	"auto":          {},
	"high_contrast": {},
}

// themeCookieName is read by the inline script in _layout.html before any
// CSS computes, avoiding a flash on first paint.
const themeCookieName = "theme"

// settingsAppearanceForm renders GET /settings/appearance.
func (h *Handlers) settingsAppearanceForm(w http.ResponseWriter, r *http.Request) {
	h.renderAppearanceForm(w, r, "", "")
}

// settingsAppearanceSubmit handles POST /settings/appearance.
//
// Two writes:
//  1. users.theme — source of truth across devices.
//  2. theme cookie — so the next request paints the right theme without
//     waiting on the inline JS to fall back to system preference.
func (h *Handlers) settingsAppearanceSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	choice := r.PostFormValue("theme")
	if _, ok := allowedThemes[choice]; !ok {
		h.renderAppearanceForm(w, r, "Unknown theme.", "")
		return
	}

	if err := h.q.UpdateUserTheme(r.Context(), h.d.Pool, usersdb.UpdateUserThemeParams{
		ID:    user.ID,
		Theme: choice,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "appearance: update", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	//nolint:gosec // G124 false positive: HttpOnly intentionally false because the
	// inline script in _layout.html reads this cookie before any CSS computes,
	// avoiding a flash of unthemed content. The cookie carries no auth value.
	cookie := &http.Cookie{
		Name:     themeCookieName,
		Value:    choice,
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 365, // 1 year
		HttpOnly: false,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	}
	if choice == "" {
		// Empty value clears the cookie so the inline script falls back
		// to system preference on the next paint.
		cookie.Value = ""
		cookie.MaxAge = -1
	}
	http.SetCookie(w, cookie)

	h.renderAppearanceForm(w, r, "", "Appearance updated.")
}

func (h *Handlers) renderAppearanceForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	current := ""
	if row, err := h.q.GetUserByID(r.Context(), h.d.Pool, user.ID); err == nil {
		current = row.Theme
	}
	h.renderPage(w, r, "settings/appearance", map[string]any{
		"Title":          "Appearance",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "appearance",
		"CurrentTheme":   current,
		"Error":          errMsg,
		"Success":        successMsg,
	})
}
