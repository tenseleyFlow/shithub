// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// deletionGraceWindow is the period during which a soft-deleted account
// can be restored simply by signing in. Beyond this point the row stays
// soft-deleted and the login path treats the user as nonexistent.
const deletionGraceWindow = 14 * 24 * time.Hour

// settingsDangerForm renders GET /settings/danger.
func (h *Handlers) settingsDangerForm(w http.ResponseWriter, r *http.Request) {
	h.renderDangerForm(w, r, "")
}

// settingsDangerDelete handles POST /settings/danger.
//
// Requires the user to retype their username and current password so a
// stolen session can't trigger this. On success: SoftDeleteUser, audit,
// bump session_epoch (kicks every other live session), clear THIS
// session cookie, redirect to a goodbye page.
func (h *Handlers) settingsDangerDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	confirmName := strings.ToLower(strings.TrimSpace(r.PostFormValue("confirm_username")))
	pw := r.PostFormValue("password")

	if confirmName != user.Username {
		h.renderDangerForm(w, r, "Type your username exactly to confirm.")
		return
	}
	row, err := h.q.GetUserByID(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "danger: load user", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	ok, err := password.Verify(pw, row.PasswordHash)
	if err != nil || !ok {
		h.renderDangerForm(w, r, "Password is incorrect.")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "danger: begin", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.SoftDeleteUser(r.Context(), tx, user.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "danger: soft delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.q.BumpUserSessionEpoch(r.Context(), tx, user.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "danger: bump epoch", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "danger: commit", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionAccountDeleted, audit.TargetUser, user.ID, map[string]any{
			"grace_days": int(deletionGraceWindow / (24 * time.Hour)),
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "danger: audit", "error", err)
	}

	// Sign out THIS browser. Even if the cookie weren't cleared, the
	// epoch bump above would invalidate it on the next request.
	h.d.SessionStore.Clear(w)

	http.Redirect(w, r, "/?notice=account-deleted", http.StatusSeeOther)
}

// renderDangerForm is the shared render path.
func (h *Handlers) renderDangerForm(w http.ResponseWriter, r *http.Request, errMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	h.renderPage(w, r, "settings/danger", map[string]any{
		"Title":           "Delete account",
		"CSRFToken":       middleware.CSRFTokenForRequest(r),
		"SettingsActive":  "danger",
		"Username":        user.Username,
		"GraceWindowDays": int(deletionGraceWindow / (24 * time.Hour)),
		"Error":           errMsg,
	})
}
