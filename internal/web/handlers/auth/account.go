// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// usernameChangeWindow is the rolling rate-limit window for renames.
// Counted against username_redirects.changed_at — the redirect row IS
// the audit trail, so no separate counter table is needed.
const usernameChangeWindow = 60 * 24 * time.Hour

// usernameChangeMax caps how many renames a user can make in
// usernameChangeWindow. Three is GitHub's posted ceiling.
const usernameChangeMax = 3

// settingsAccountForm renders GET /settings/account.
func (h *Handlers) settingsAccountForm(w http.ResponseWriter, r *http.Request) {
	h.renderAccountForm(w, r, "", "")
}

// settingsAccountUsername handles POST /settings/account/username.
func (h *Handlers) settingsAccountUsername(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	desired := strings.ToLower(strings.TrimSpace(r.PostFormValue("new_username")))

	// Quick local checks: shape, reservation, no-op.
	if desired == user.Username {
		h.renderAccountForm(w, r, "That's already your username.", "")
		return
	}
	if !usernameRE.MatchString(desired) {
		h.renderAccountForm(w, r, "Username must be 1–39 characters: lowercase letters, digits, and hyphens (no leading/trailing hyphen).", "")
		return
	}
	if authpkg.IsReserved(desired) {
		h.renderAccountForm(w, r, "That username is reserved.", "")
		return
	}

	// Rate limit.
	count, err := h.q.CountRecentUsernameChanges(r.Context(), h.d.Pool, usersdb.CountRecentUsernameChangesParams{
		UserID:    user.ID,
		ChangedAt: pgtype.Timestamptz{Time: time.Now().Add(-usernameChangeWindow), Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "account: count renames", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if count >= usernameChangeMax {
		h.renderAccountForm(w, r, "You've changed your username too many times recently. Try again later.", "")
		return
	}

	// Tx: redirect-row + rename. The unique constraint on
	// username_redirects.old_username AND users.username (citext) blocks
	// taking a name held by either an active user or a recent redirect.
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "account: begin tx", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.InsertUsernameRedirect(r.Context(), tx, usersdb.InsertUsernameRedirectParams{
		OldUsername: user.Username,
		UserID:      user.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "account: insert redirect", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.q.RenameUser(r.Context(), tx, usersdb.RenameUserParams{
		ID:       user.ID,
		Username: desired,
	}); err != nil {
		if isUsernameTaken(err) {
			h.renderAccountForm(w, r, "That username is taken.", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "account: rename", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "account: commit", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.d.Audit.Record(r.Context(), h.d.Pool, user.ID,
		audit.ActionUsernameChanged, audit.TargetUser, user.ID, map[string]any{
			"from": user.Username,
			"to":   desired,
		}); err != nil {
		h.d.Logger.WarnContext(r.Context(), "account: audit rename", "error", err)
	}

	h.notifyState(r.Context(), user.ID, "username_changed")

	h.renderAccountForm(w, r, "", "Username updated to "+desired+".")
}

// renderAccountForm is the shared render path. The current username is
// pulled out of context so it reflects post-rename state on success.
func (h *Handlers) renderAccountForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	// Re-read the canonical username from the DB so the form always
	// reflects post-commit state when called after a successful rename.
	canonical := user.Username
	if row, err := h.q.GetUserByID(r.Context(), h.d.Pool, user.ID); err == nil {
		canonical = row.Username
	}

	count, _ := h.q.CountRecentUsernameChanges(r.Context(), h.d.Pool, usersdb.CountRecentUsernameChangesParams{
		UserID:    user.ID,
		ChangedAt: pgtype.Timestamptz{Time: time.Now().Add(-usernameChangeWindow), Valid: true},
	})
	h.renderPage(w, r, "settings/account", map[string]any{
		"Title":             "Account",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"SettingsActive":    "account",
		"CurrentUsername":   canonical,
		"RecentRenames":     count,
		"MaxRenames":        usernameChangeMax,
		"WindowDays":        int(usernameChangeWindow / (24 * time.Hour)),
		"RenameRateLimited": count >= usernameChangeMax,
		"Error":             errMsg,
		"Success":           successMsg,
	})
}

// isUsernameTaken matches the unique-violation surface for username collisions.
// Both users.username (citext, unique) and username_redirects.old_username
// (unique) raise SQLSTATE 23505 here.
func isUsernameTaken(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	// pgx v5 also wraps tx errors; double-check the not-rows path.
	return errors.Is(err, pgx.ErrNoRows)
}
