// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/token"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// maxEmailsPerUser caps how many addresses a user can register. Picked
// to match GitHub's posted ceiling — high enough that nobody hits it
// legitimately, low enough that an abuser can't use the table as scratch
// space.
const maxEmailsPerUser = 10

// settingsEmailsList renders GET /settings/emails.
func (h *Handlers) settingsEmailsList(w http.ResponseWriter, r *http.Request) {
	h.renderEmailsList(w, r, "", "")
}

// settingsEmailsAdd handles POST /settings/emails.
func (h *Handlers) settingsEmailsAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	addr := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))

	if !looksLikeEmail(addr) {
		h.renderEmailsList(w, r, "Please enter a valid email address.", "")
		return
	}

	count, err := h.countUserEmails(r, user.ID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if count >= maxEmailsPerUser {
		h.renderEmailsList(w, r, "You've reached the per-account email cap. Remove one first.", "")
		return
	}

	tokEnc, tokHash, err := token.New()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: token", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: begin", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	em, err := h.q.CreateUserEmail(r.Context(), tx, usersdb.CreateUserEmailParams{
		UserID:                user.ID,
		Email:                 addr,
		IsPrimary:             false,
		Verified:              false,
		VerificationTokenHash: tokHash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			h.renderEmailsList(w, r, "That email is already registered (here or to another account).", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "emails: insert", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	if _, err := h.q.CreateEmailVerification(r.Context(), tx, usersdb.CreateEmailVerificationParams{
		UserEmailID: em.ID, TokenHash: tokHash, ExpiresAt: expires,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: verification row", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: commit", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.sendVerifyMessage(r, addr, user.Username, tokEnc)
	h.renderEmailsList(w, r, "", "Verification link sent to "+addr+".")
}

// settingsEmailsResend handles POST /settings/emails/{id}/resend.
func (h *Handlers) settingsEmailsResend(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseEmailID(w, r)
	if !ok {
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, id)
	if err != nil || em.UserID != user.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if em.Verified {
		h.renderEmailsList(w, r, "That address is already verified.", "")
		return
	}

	tokEnc, tokHash, err := token.New()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: resend token", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if err := h.q.SetVerificationToken(r.Context(), tx, usersdb.SetVerificationTokenParams{
		ID: em.ID, VerificationTokenHash: tokHash,
	}); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if _, err := h.q.CreateEmailVerification(r.Context(), tx, usersdb.CreateEmailVerificationParams{
		UserEmailID: em.ID,
		TokenHash:   tokHash,
		ExpiresAt:   pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true},
	}); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.sendVerifyMessage(r, string(em.Email), user.Username, tokEnc)
	h.renderEmailsList(w, r, "", "Verification link resent to "+string(em.Email)+".")
}

// settingsEmailsSetPrimary handles POST /settings/emails/{id}/primary.
func (h *Handlers) settingsEmailsSetPrimary(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseEmailID(w, r)
	if !ok {
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, id)
	if err != nil || em.UserID != user.ID {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	if !em.Verified {
		h.renderEmailsList(w, r, "Verify the address before promoting it to primary.", "")
		return
	}
	if em.IsPrimary {
		h.renderEmailsList(w, r, "That address is already primary.", "")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if err := h.q.SetUserEmailPrimary(r.Context(), tx, usersdb.SetUserEmailPrimaryParams{
		UserID: user.ID, ID: em.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: set primary", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.q.LinkUserPrimaryEmail(r.Context(), tx, usersdb.LinkUserPrimaryEmailParams{
		ID:             user.ID,
		PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: link primary", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.notifyState(r.Context(), user.ID, "primary_email_changed")

	h.renderEmailsList(w, r, "", "Primary email is now "+string(em.Email)+".")
}

// settingsEmailsRemove handles POST /settings/emails/{id}/remove.
func (h *Handlers) settingsEmailsRemove(w http.ResponseWriter, r *http.Request) {
	id, ok := h.parseEmailID(w, r)
	if !ok {
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.DeleteUserEmail(r.Context(), h.d.Pool, usersdb.DeleteUserEmailParams{
		ID: id, UserID: user.ID,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if rows == 0 {
		h.renderEmailsList(w, r, "Couldn't remove that email — set a different primary first.", "")
		return
	}
	h.renderEmailsList(w, r, "", "Email removed.")
}

// renderEmailsList is the shared render path.
func (h *Handlers) renderEmailsList(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListUserEmailsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderPage(w, r, "settings/emails", map[string]any{
		"Title":          "Emails",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "emails",
		"Emails":         rows,
		"Error":          errMsg,
		"Success":        successMsg,
	})
}

// parseEmailID extracts and validates the {id} route param. On failure
// writes a 404 and returns ok=false.
func (h *Handlers) parseEmailID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return 0, false
	}
	return id, true
}

func (h *Handlers) countUserEmails(r *http.Request, userID int64) (int, error) {
	rows, err := h.q.ListUserEmailsForUser(r.Context(), h.d.Pool, userID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "emails: count", "error", err)
		return 0, err
	}
	return len(rows), nil
}

// sendVerifyMessage is best-effort: failures don't break the flow but
// are logged so the operator can chase delivery issues.
func (h *Handlers) sendVerifyMessage(r *http.Request, addr, username, tokEnc string) {
	msg, err := email.VerifyMessage(h.d.Branding, addr, username, tokEnc)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "emails: build verify msg", "error", err)
		return
	}
	if err := h.d.Email.Send(r.Context(), msg); err != nil {
		h.d.Logger.WarnContext(r.Context(), "emails: send verify msg", "error", err)
	}
}
