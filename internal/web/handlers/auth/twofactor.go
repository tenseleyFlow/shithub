// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/auth/totp"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// ============================ login challenge ===========================

func (h *Handlers) twoFactorChallengeForm(w http.ResponseWriter, r *http.Request) {
	s := middleware.SessionFromContext(r.Context())
	if s.Pre2FAUserID == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	h.renderPage(w, r, "auth/2fa_challenge", map[string]any{
		"Title":     "Two-factor authentication",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Next":      r.URL.Query().Get("next"),
	})
}

func (h *Handlers) twoFactorChallengeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	s := middleware.SessionFromContext(r.Context())
	if s.Pre2FAUserID == 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	userID := s.Pre2FAUserID
	code := strings.TrimSpace(r.PostFormValue("code"))
	next := r.PostFormValue("next")

	render := func(msg string) {
		h.renderPage(w, r, "auth/2fa_challenge", map[string]any{
			"Title":     "Two-factor authentication",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Error":     msg,
			"Next":      next,
		})
	}

	throttleKey := fmt.Sprintf("ip:%s|uid:%d", clientIP(r), userID)
	if err := h.d.Limiter.Hit(r.Context(), h.d.Pool, throttle.Limit{
		Scope: "2fa", Identifier: throttleKey,
		Max: 5, Window: 5 * time.Minute,
	}); err != nil {
		h.writeRetryAfter(w, err)
		render("Too many failed attempts. Please sign in again.")
		// Drop pre-2fa marker so caller restarts the flow.
		s.Pre2FAUserID = 0
		_ = h.d.SessionStore.Save(w, r, s)
		return
	}

	if code == "" {
		render("Enter your 6-digit code or a recovery code.")
		return
	}

	accepted := false
	usedRecovery := false
	if totp.LooksLikeRecoveryCode(code) {
		ok, err := h.consumeRecoveryCode(r.Context(), userID, code)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "2fa: consume recovery", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		accepted = ok
		usedRecovery = ok
	} else {
		ok, err := h.verifyTOTPCode(r.Context(), userID, code)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "2fa: verify totp", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		accepted = ok
	}

	if !accepted {
		render("Incorrect code. Try again.")
		return
	}

	// Forgive prior failed-attempt counter on success.
	_ = h.d.Limiter.Reset(r.Context(), h.d.Pool, "2fa", throttleKey)

	if err := h.q.TouchUserLastLogin(r.Context(), h.d.Pool, userID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "2fa: touch last_login_at", "error", err)
	}

	if usedRecovery {
		_ = h.d.Audit.Record(r.Context(), h.d.Pool, userID,
			audit.ActionRecoveryCodeUsed, audit.TargetUser, userID, nil)
	}

	// Upgrade session: drop pre-2FA marker, set UserID, reissue.
	s.Pre2FAUserID = 0
	s.UserID = userID
	s.IssuedAt = time.Now().Unix()
	if err := h.d.SessionStore.Save(w, r, s); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: save session", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	dest := "/"
	if next != "" && strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		dest = next
	}
	//nolint:gosec // G710: dest is whitelisted to single-leading-slash relative paths.
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ============================ enrollment ================================

func (h *Handlers) twoFactorEnableForm(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())

	// If already enrolled and confirmed, send to disable page instead.
	if existing, err := h.q.GetUserTOTP(r.Context(), h.d.Pool, user.ID); err == nil && existing.ConfirmedAt.Valid {
		http.Redirect(w, r, "/settings/security/2fa/disable", http.StatusSeeOther)
		return
	}

	// Mint or replace a pending secret. UpsertUserTOTP only updates when
	// confirmed_at IS NULL — confirmed rows are protected.
	secret, err := totp.GenerateSecret()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: secret", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	enc, nonce, err := h.d.SecretBox.Seal(secret)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: seal", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if _, err := h.q.UpsertUserTOTP(r.Context(), h.d.Pool, usersdb.UpsertUserTOTPParams{
		UserID:          user.ID,
		SecretEncrypted: enc,
		SecretNonce:     nonce,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: upsert secret", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	uri := totp.OtpauthURI(h.d.Branding.SiteName, user.Username, secret)
	svg, err := totp.QRSVG(uri)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: qr", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.renderPage(w, r, "settings/2fa_enable", map[string]any{
		"Title":     "Enable two-factor authentication",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"QRSvg":     svg,
		"Secret":    totp.EncodeBase32(secret), // displayed for manual entry; also high-entropy + redacted in logs
	})
}

func (h *Handlers) twoFactorEnableSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	code := strings.TrimSpace(r.PostFormValue("code"))

	render := func(msg string, recoveryCodes []string) {
		data := map[string]any{
			"Title":     "Enable two-factor authentication",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
		}
		if msg != "" {
			data["Error"] = msg
		}
		if len(recoveryCodes) > 0 {
			data["RecoveryCodes"] = recoveryCodes
		}
		h.renderPage(w, r, "settings/2fa_recovery", data)
	}

	row, err := h.q.GetUserTOTP(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "no pending 2FA enrollment")
		return
	}
	if row.ConfirmedAt.Valid {
		http.Redirect(w, r, "/settings/security/2fa/disable", http.StatusSeeOther)
		return
	}

	secret, err := h.d.SecretBox.Open(row.SecretEncrypted, row.SecretNonce)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: open secret", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	step, err := totp.Verify(secret, code, time.Now())
	if err != nil {
		render("That code is incorrect. Try again.", nil)
		return
	}

	codes, hashes, err := totp.GenerateRecoveryCodes()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: generate recovery", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	// ConfirmUserTOTP only updates when confirmed_at IS NULL — handles the
	// parallel-enrollment race; a second submit finds rows-affected==0.
	rows, err := h.q.ConfirmUserTOTP(r.Context(), tx, usersdb.ConfirmUserTOTPParams{
		UserID:          user.ID,
		LastUsedCounter: step,
	})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if rows == 0 {
		// Already confirmed by a parallel request.
		http.Redirect(w, r, "/settings/security/2fa/disable", http.StatusSeeOther)
		return
	}

	if err := h.q.DeleteUserRecoveryCodes(r.Context(), tx, user.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	for _, hsh := range hashes {
		if err := h.q.InsertRecoveryCode(r.Context(), tx, usersdb.InsertRecoveryCodeParams{
			UserID: user.ID, CodeHash: hsh,
		}); err != nil {
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	if err := h.d.Audit.Record(r.Context(), tx, user.ID,
		audit.Action2FAEnabled, audit.TargetUser, user.ID, nil); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.d.Audit.Record(r.Context(), tx, user.ID,
		audit.ActionRecoveryCodesIssued, audit.TargetUser, user.ID, map[string]any{"count": len(codes)}); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.notifyUser(r.Context(), user.ID, "2fa_enabled")

	render("", codes)
}

// =============================== disable ================================

func (h *Handlers) twoFactorDisableForm(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, "settings/2fa_disable", map[string]any{
		"Title":     "Disable two-factor authentication",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
	})
}

func (h *Handlers) twoFactorDisableSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	pw := r.PostFormValue("password")
	code := strings.TrimSpace(r.PostFormValue("code"))

	render := func(msg string) {
		h.renderPage(w, r, "settings/2fa_disable", map[string]any{
			"Title":     "Disable two-factor authentication",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Error":     msg,
		})
	}

	if ok, err := h.confirmPasswordAndTOTP(r.Context(), user.ID, pw, code); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: disable confirm", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	} else if !ok {
		render("Password or code incorrect. Please try again.")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.DeleteUserTOTP(r.Context(), tx, user.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.q.DeleteUserRecoveryCodes(r.Context(), tx, user.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.d.Audit.Record(r.Context(), tx, user.ID,
		audit.Action2FADisabled, audit.TargetUser, user.ID, nil); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.notifyUser(r.Context(), user.ID, "2fa_disabled")
	http.Redirect(w, r, "/settings/security/2fa/enable?notice=disabled", http.StatusSeeOther)
}

// ============================== regenerate ==============================

func (h *Handlers) twoFactorRegenerateSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	pw := r.PostFormValue("password")
	code := strings.TrimSpace(r.PostFormValue("code"))

	if ok, err := h.confirmPasswordAndTOTP(r.Context(), user.ID, pw, code); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "2fa: regen confirm", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	} else if !ok {
		h.d.Render.HTTPError(w, r, http.StatusUnauthorized, "Password or code incorrect")
		return
	}

	codes, hashes, err := totp.GenerateRecoveryCodes()
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.DeleteUserRecoveryCodes(r.Context(), tx, user.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	for _, hsh := range hashes {
		if err := h.q.InsertRecoveryCode(r.Context(), tx, usersdb.InsertRecoveryCodeParams{
			UserID: user.ID, CodeHash: hsh,
		}); err != nil {
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	if err := h.d.Audit.Record(r.Context(), tx, user.ID,
		audit.ActionRecoveryRegenerated, audit.TargetUser, user.ID, map[string]any{"count": len(codes)}); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	h.notifyUser(r.Context(), user.ID, "recovery_regenerated")

	h.renderPage(w, r, "settings/2fa_recovery", map[string]any{
		"Title":         "New recovery codes",
		"CSRFToken":     middleware.CSRFTokenForRequest(r),
		"RecoveryCodes": codes,
	})
}

// =============================== helpers =================================

// verifyTOTPCode verifies code against the user's confirmed TOTP secret
// AND advances last_used_counter atomically (counter anti-replay).
func (h *Handlers) verifyTOTPCode(ctx context.Context, userID int64, code string) (bool, error) {
	row, err := h.q.GetUserTOTP(ctx, h.d.Pool, userID)
	if err != nil {
		return false, nil // no enrollment → reject without leaking
	}
	if !row.ConfirmedAt.Valid {
		return false, nil
	}
	secret, err := h.d.SecretBox.Open(row.SecretEncrypted, row.SecretNonce)
	if err != nil {
		return false, fmt.Errorf("open secret: %w", err)
	}
	step, err := totp.Verify(secret, code, time.Now())
	if err != nil {
		return false, nil
	}
	rows, err := h.q.BumpTOTPCounter(ctx, h.d.Pool, usersdb.BumpTOTPCounterParams{
		UserID:          userID,
		LastUsedCounter: step,
	})
	if err != nil {
		return false, fmt.Errorf("bump counter: %w", err)
	}
	return rows == 1, nil // rows==0 means counter replay → reject
}

// consumeRecoveryCode hashes the typed code and tries to mark it used.
// Returns true iff exactly one matching unused row was found.
func (h *Handlers) consumeRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	hash := totp.HashRecoveryCode(code)
	rows, err := h.q.ConsumeRecoveryCode(ctx, h.d.Pool, usersdb.ConsumeRecoveryCodeParams{
		UserID: userID, CodeHash: hash,
	})
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// confirmPasswordAndTOTP validates current password AND current TOTP
// before sensitive 2FA state changes. Returns (true, nil) on success,
// (false, nil) on a clean rejection, or (false, err) on a real error.
func (h *Handlers) confirmPasswordAndTOTP(ctx context.Context, userID int64, pw, code string) (bool, error) {
	user, err := h.q.GetUserByID(ctx, h.d.Pool, userID)
	if err != nil {
		return false, err
	}
	ok, err := password.Verify(pw, user.PasswordHash)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if codeOK, err := h.verifyTOTPCode(ctx, userID, code); err != nil {
		return false, err
	} else if !codeOK {
		return false, nil
	}
	return true, nil
}

// notifyUser sends a notification email about a 2FA state change. Best
// effort — failure is logged but does not break the flow.
func (h *Handlers) notifyUser(ctx context.Context, userID int64, kind string) {
	user, err := h.q.GetUserByID(ctx, h.d.Pool, userID)
	if err != nil {
		return
	}
	if !user.PrimaryEmailID.Valid {
		return
	}
	em, err := h.q.GetUserEmailByID(ctx, h.d.Pool, user.PrimaryEmailID.Int64)
	if err != nil {
		return
	}
	msg, err := email.NoticeMessage(h.d.Branding, string(em.Email), user.Username, kind)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "notice: build", "kind", kind, "error", err)
		return
	}
	if err := h.d.Email.Send(ctx, msg); err != nil {
		h.d.Logger.WarnContext(ctx, "notice: send", "kind", kind, "error", err)
	}
}

// silence unused import warnings if guards are removed.
var (
	_ = pgx.ErrNoRows
	_ = pgtype.Int8{}
	_ = errors.New
)
