// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"time"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// settingsSessionsList renders GET /settings/sessions.
//
// V1 surfaces only the current session (we use AEAD-encrypted cookies,
// no server-side session table — there's no enumerable list). The page's
// real value is the "Sign out everywhere" affordance, which bumps
// users.session_epoch and invalidates every cookie that carries the old
// epoch on its next request.
func (h *Handlers) settingsSessionsList(w http.ResponseWriter, r *http.Request) {
	h.renderSessionsList(w, r, "")
}

// settingsSessionsLogoutAll handles POST /settings/sessions/logout-everywhere.
func (h *Handlers) settingsSessionsLogoutAll(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())

	if err := h.q.BumpUserSessionEpoch(r.Context(), h.d.Pool, user.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sessions: bump", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	// Re-sync the current session's epoch so this browser doesn't sign
	// itself out alongside the others.
	epoch, err := h.q.GetUserSessionEpoch(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sessions: read epoch", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	s := middleware.SessionFromContext(r.Context())
	s.Epoch = epoch
	if err := h.d.SessionStore.Save(w, r, s); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "sessions: save", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.notifyState(r.Context(), user.ID, "log_out_everywhere")

	h.renderSessionsList(w, r, "Signed out of every other session.")
}

// renderSessionsList is the shared render path. The "current session"
// row pulls IssuedAt out of the loaded session struct.
func (h *Handlers) renderSessionsList(w http.ResponseWriter, r *http.Request, successMsg string) {
	s := middleware.SessionFromContext(r.Context())
	issued := time.Unix(s.IssuedAt, 0)

	h.renderPage(w, r, "settings/sessions", map[string]any{
		"Title":          "Sessions",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "sessions",
		"Issued":         issued,
		"UserAgent":      r.Header.Get("User-Agent"),
		"ClientIP":       clientIP(r),
		"Success":        successMsg,
	})
}
