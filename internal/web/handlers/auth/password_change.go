// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/passwords"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// settingsPasswordForm renders GET /settings/password.
func (h *Handlers) settingsPasswordForm(w http.ResponseWriter, r *http.Request) {
	h.renderPasswordForm(w, r, "", "")
}

// settingsPasswordSubmit handles POST /settings/password.
//
// Flow:
//  1. Recent-2FA gate (handlers/auth/tokens.go::recentAuthOK semantics).
//     Skipped for users without 2FA enrolled.
//  2. Verify the current password.
//  3. Validate the new password against the same rules as signup.
//  4. Hash + UpdateUserPassword + bump session_epoch (the bump invalidates
//     all OTHER sessions on the account; the current session sticks
//     because we re-issue its cookie below).
//  5. Audit-log + (later) email notification.
func (h *Handlers) settingsPasswordSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	current := r.PostFormValue("current_password")
	next := r.PostFormValue("new_password")
	confirm := r.PostFormValue("confirm_password")

	if !h.recentAuthOK(r) {
		h.renderPasswordForm(w, r,
			"Confirm 2FA recently before changing your password: sign in again with your authenticator code.",
			"")
		return
	}

	row, err := h.q.GetUserByID(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "password: load user", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	ok, err := password.Verify(current, row.PasswordHash)
	if err != nil || !ok {
		h.renderPasswordForm(w, r, "Current password is incorrect.", "")
		return
	}

	if msg := validateNewPassword(next, confirm); msg != "" {
		h.renderPasswordForm(w, r, msg, "")
		return
	}
	if next == current {
		h.renderPasswordForm(w, r, "Pick a password different from your current one.", "")
		return
	}

	hash, err := hashPassword(next, h.d.Argon2)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "password: hash", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "password: begin", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.UpdateUserPassword(r.Context(), tx, usersdb.UpdateUserPasswordParams{
		ID: user.ID, PasswordHash: hash, PasswordAlgo: "argon2id-v1",
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "password: update", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.q.BumpUserSessionEpoch(r.Context(), tx, user.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "password: bump epoch", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "password: commit", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionPasswordChanged, audit.TargetUser, user.ID, map[string]any{
			"via": "settings",
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "password: audit", "error", err)
	}

	h.notifyState(r.Context(), user.ID, "password_changed")

	// Sync the *current* session's epoch with the post-bump value so the
	// user staying on this browser doesn't get signed out by their own
	// password change.
	if epoch, err := h.q.GetUserSessionEpoch(r.Context(), h.d.Pool, user.ID); err == nil {
		s := middleware.SessionFromContext(r.Context())
		s.Epoch = epoch
		if err := h.d.SessionStore.Save(w, r, s); err != nil {
			h.d.Logger.WarnContext(r.Context(), "password: re-issue session", "error", err)
		}
	}

	h.renderPasswordForm(w, r, "", "Password updated. Other sessions have been signed out.")
}

func (h *Handlers) renderPasswordForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	h.renderPage(w, r, "settings/password", map[string]any{
		"Title":          "Password",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "password",
		"RecentAuthOK":   h.recentAuthOK(r),
		"Error":          errMsg,
		"Success":        successMsg,
	})
}

// validateNewPassword enforces signup parity for the chosen new password.
func validateNewPassword(next, confirm string) string {
	if len(next) < 10 {
		return "Password must be at least 10 characters."
	}
	if next != confirm {
		return "New password and confirmation don't match."
	}
	if passwords.IsCommon(next) {
		return "That password is too common. Please choose another."
	}
	return ""
}
