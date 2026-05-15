// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-07b — settings → scheduled issues. Read-only list of the
// user's scheduled-issue rows plus a cancel form for pending ones.

func (h *Handlers) settingsScheduledIssuesForm(w http.ResponseWriter, r *http.Request) {
	h.renderScheduledIssuesForm(w, r, "", "")
}

func (h *Handlers) settingsScheduledIssueCancel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderScheduledIssuesForm(w, r, "Invalid scheduled-issue id.", "")
		return
	}
	if err := h.q.CancelScheduledIssue(r.Context(), h.d.Pool, usersdb.CancelScheduledIssueParams{
		ID:     id,
		UserID: user.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/scheduled-issues: cancel", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	// Note: the worker job is still enqueued at the original run_at.
	// When it fires, the handler sees status='cancelled' and short-
	// circuits. We don't reach into the jobs table to delete the row —
	// the worker's own short-circuit is the source of truth.
	h.renderScheduledIssuesForm(w, r, "", "Schedule cancelled.")
}

func (h *Handlers) renderScheduledIssuesForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListScheduledIssuesForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings/scheduled-issues: list", "user_id", user.ID, "error", err)
	}
	h.renderPage(w, r, "settings/scheduled_issues", map[string]any{
		"Title":          "Scheduled issues",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "scheduled-issues",
		"Rows":           rows,
		"Error":          errMsg,
		"Success":        successMsg,
	})
}
