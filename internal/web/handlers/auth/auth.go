// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth wires the email/password auth handlers (signup, login,
// logout, password reset, email verification) onto the chi router.
//
// Design points worth knowing:
//   - Login is constant-time: when the username doesn't exist we still
//     run a Verify against a pre-computed dummy hash so response time
//     doesn't leak existence.
//   - Password-reset returns the same generic notice whether or not the
//     email maps to a real account (no enumeration via the reset flow).
//   - Tokens (verification, reset) are 32-byte random, b64url-encoded for
//     the URL, sha256-stored in the DB.
//   - Rate limits are enforced via internal/auth/throttle (login, signup,
//     password-reset). Login on success resets the counter for that key.
//   - Honeypot: signup form has a hidden `company` field; non-empty
//     submissions are silently dropped (200 + same notice as success so
//     bots can't tell they were rejected).
//   - 4 KiB request-body cap on every POST here so weaponized inputs
//     can't push the argon2 hasher into a DoS spiral.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/auth/password"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/auth/token"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/passwords"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// usernameRE mirrors the path whitelist used by RepoFS.
var usernameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)

// Deps is everything the auth handlers need. Constructed by the web
// package and injected at registration time.
type Deps struct {
	Logger                   *slog.Logger
	Render                   *render.Renderer
	Pool                     *pgxpool.Pool
	SessionStore             session.Store
	Email                    email.Sender
	Branding                 email.Branding
	Argon2                   password.Params
	Limiter                  *throttle.Limiter
	RequireEmailVerification bool
	// SecretBox encrypts at-rest TOTP secrets. May be nil; when nil, the
	// 2FA enrollment endpoints are not registered.
	SecretBox *secretbox.Box
	// Audit records security-relevant events (2fa state changes, etc.).
	Audit *audit.Recorder
	// ObjectStore is the avatar / attachment backend. May be nil; when
	// nil the avatar upload endpoint is not registered (the user keeps
	// their identicon and never sees the upload form).
	ObjectStore storage.ObjectStore
}

// Handlers is the registered handler set. Construct with New.
type Handlers struct {
	d Deps
	q *usersdb.Queries
}

// New constructs the handler set, validating Deps.
func New(d Deps) (*Handlers, error) {
	if d.Render == nil {
		return nil, errors.New("auth: nil Render")
	}
	if d.SessionStore == nil {
		return nil, errors.New("auth: nil SessionStore")
	}
	if d.Email == nil {
		return nil, errors.New("auth: nil Email sender")
	}
	if d.Limiter == nil {
		d.Limiter = throttle.NewLimiter()
	}
	if d.Audit == nil {
		d.Audit = audit.NewRecorder()
	}
	password.MustGenerateDummy(d.Argon2)
	return &Handlers{d: d, q: usersdb.New()}, nil
}

// Mount registers every auth route on r. The 4 KiB body cap is applied
// to POST endpoints inside this method.
func (h *Handlers) Mount(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.MaxBodySize(4 * 1024))
		r.Get("/signup", h.signupForm)
		r.Post("/signup", h.signupSubmit)
		r.Get("/login", h.loginForm)
		r.Post("/login", h.loginSubmit)
		r.Get("/login/2fa", h.twoFactorChallengeForm)
		r.Post("/login/2fa", h.twoFactorChallengeSubmit)
		r.Post("/logout", h.logoutSubmit)
		r.Get("/password/reset", h.resetRequestForm)
		r.Post("/password/reset", h.resetRequestSubmit)
		r.Get("/password/reset/{token}", h.resetConfirmForm)
		r.Post("/password/reset/{token}", h.resetConfirmSubmit)
		r.Get("/verify-email/{token}", h.verifyEmail)
		r.Get("/verify-email/resend", h.verifyResendForm)
		r.Post("/verify-email/resend", h.verifyResendSubmit)

		// Settings — require an authenticated user.
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireUser)
			r.Get("/settings/profile", h.settingsProfileForm)
			r.Post("/settings/profile", h.settingsProfileSubmit)
			if h.d.ObjectStore != nil {
				r.Post("/settings/profile/avatar", h.settingsAvatarUpload)
				r.Post("/settings/profile/avatar/remove", h.settingsAvatarRemove)
			}
			r.Get("/settings/account", h.settingsAccountForm)
			r.Post("/settings/account/username", h.settingsAccountUsername)
			r.Get("/settings/password", h.settingsPasswordForm)
			r.Post("/settings/password", h.settingsPasswordSubmit)
			r.Get("/settings/appearance", h.settingsAppearanceForm)
			r.Post("/settings/appearance", h.settingsAppearanceSubmit)
			r.Get("/settings/emails", h.settingsEmailsList)
			r.Post("/settings/emails", h.settingsEmailsAdd)
			r.Post("/settings/emails/{id}/resend", h.settingsEmailsResend)
			r.Post("/settings/emails/{id}/primary", h.settingsEmailsSetPrimary)
			r.Post("/settings/emails/{id}/remove", h.settingsEmailsRemove)
			r.Get("/settings/notifications", h.settingsNotificationsForm)
			r.Post("/settings/notifications", h.settingsNotificationsSubmit)
			r.Get("/settings/sessions", h.settingsSessionsList)
			r.Post("/settings/sessions/logout-everywhere", h.settingsSessionsLogoutAll)
			r.Get("/settings/danger", h.settingsDangerForm)
			r.Post("/settings/danger", h.settingsDangerDelete)
			r.Get("/settings/keys", h.sshKeysList)
			r.Post("/settings/keys", h.sshKeysAdd)
			r.Post("/settings/keys/{id}/delete", h.sshKeysDelete)
			r.Get("/settings/tokens", h.tokensList)
			r.Post("/settings/tokens", h.tokensCreate)
			r.Post("/settings/tokens/{id}/revoke", h.tokensRevoke)
			if h.d.SecretBox != nil {
				r.Get("/settings/security/2fa/enable", h.twoFactorEnableForm)
				r.Post("/settings/security/2fa/enable", h.twoFactorEnableSubmit)
				r.Get("/settings/security/2fa/disable", h.twoFactorDisableForm)
				r.Post("/settings/security/2fa/disable", h.twoFactorDisableSubmit)
				r.Post("/settings/security/2fa/regenerate", h.twoFactorRegenerateSubmit)
			}
		})
	})
}

// ---------------------------- signup -----------------------------------

type signupForm struct {
	Username string
	Email    string
}

func (h *Handlers) signupForm(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, "auth/signup", map[string]any{
		"Title":     "Sign up",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Form":      signupForm{},
	})
}

func (h *Handlers) signupSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	form := signupForm{
		Username: strings.ToLower(strings.TrimSpace(r.PostFormValue("username"))),
		Email:    strings.ToLower(strings.TrimSpace(r.PostFormValue("email"))),
	}
	password := r.PostFormValue("password")
	honeypot := r.PostFormValue("company")

	render := func(msg string) {
		h.renderPage(w, r, "auth/signup", map[string]any{
			"Title":     "Sign up",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Form":      form,
			"Error":     msg,
		})
	}

	// Honeypot: silently treat as success so bots can't probe.
	if honeypot != "" {
		http.Redirect(w, r, "/login?notice=signup-pending", http.StatusSeeOther)
		return
	}

	if err := h.throttleSignup(r); err != nil {
		h.writeRetryAfter(w, err)
		render("Too many signup attempts. Please try again later.")
		return
	}

	if msg := validateUsername(form.Username); msg != "" {
		render(msg)
		return
	}
	if !looksLikeEmail(form.Email) {
		render("Please enter a valid email address.")
		return
	}
	if len(password) < 10 {
		render("Password must be at least 10 characters.")
		return
	}
	if passwords.IsCommon(password) {
		render("That password is too common. Please choose another.")
		return
	}

	hash, err := hashPassword(password, h.d.Argon2)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "signup: hash", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	ctx := r.Context()
	tx, err := h.d.Pool.Begin(ctx)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "signup: begin tx", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	user, err := h.q.CreateUser(ctx, tx, usersdb.CreateUserParams{
		Username:     form.Username,
		DisplayName:  form.Username,
		PasswordHash: hash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			render("That username is already taken.")
			return
		}
		h.d.Logger.ErrorContext(ctx, "signup: create user", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	tokEnc, tokHash, err := token.New()
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "signup: token", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	em, err := h.q.CreateUserEmail(ctx, tx, usersdb.CreateUserEmailParams{
		UserID:                user.ID,
		Email:                 form.Email,
		IsPrimary:             true,
		Verified:              false,
		VerificationTokenHash: tokHash,
	})
	if err != nil {
		if isUniqueViolation(err) {
			render("That email is already registered. Try signing in?")
			return
		}
		h.d.Logger.ErrorContext(ctx, "signup: create email", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := h.q.LinkUserPrimaryEmail(ctx, tx, usersdb.LinkUserPrimaryEmailParams{
		ID:             user.ID,
		PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		h.d.Logger.ErrorContext(ctx, "signup: link primary", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	expires := pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	if _, err := h.q.CreateEmailVerification(ctx, tx, usersdb.CreateEmailVerificationParams{
		UserEmailID: em.ID,
		TokenHash:   tokHash,
		ExpiresAt:   expires,
	}); err != nil {
		h.d.Logger.ErrorContext(ctx, "signup: create verification", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.d.Logger.ErrorContext(ctx, "signup: commit", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	// Best-effort send. SMTP transient failure must not break signup.
	msg, err := email.VerifyMessage(h.d.Branding, form.Email, form.Username, tokEnc)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "signup: build verify msg", "error", err)
	} else if err := h.d.Email.Send(ctx, msg); err != nil {
		h.d.Logger.WarnContext(ctx, "signup: send verify email", "error", err)
	}

	http.Redirect(w, r, "/login?notice=signup-pending", http.StatusSeeOther)
}

// ----------------------------- login -----------------------------------

type loginForm struct {
	Username string
}

func (h *Handlers) loginForm(w http.ResponseWriter, r *http.Request) {
	notice := ""
	switch r.URL.Query().Get("notice") {
	case "signup-pending":
		notice = "Account created. Check your email for the verification link, then sign in."
	case "verified":
		notice = "Email verified. You can sign in now."
	case "logged-out":
		notice = "Signed out."
	case "password-reset":
		notice = "Password updated. Sign in with your new password."
	}
	h.renderPage(w, r, "auth/login", map[string]any{
		"Title":     "Sign in",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Form":      loginForm{},
		"Notice":    notice,
		"Next":      r.URL.Query().Get("next"),
	})
}

func (h *Handlers) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostFormValue("username")))
	pw := r.PostFormValue("password")
	next := r.PostFormValue("next")

	render := func(msg string) {
		h.renderPage(w, r, "auth/login", map[string]any{
			"Title":     "Sign in",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Form":      loginForm{Username: username},
			"Error":     msg,
			"Next":      next,
		})
	}

	throttleKey := fmt.Sprintf("ip:%s|%s", clientIP(r), username)
	if err := h.d.Limiter.Hit(r.Context(), h.d.Pool, throttle.Limit{
		Scope: "login", Identifier: throttleKey,
		Max: 6, Window: 15 * time.Minute,
	}); err != nil {
		h.writeRetryAfter(w, err)
		render("Too many sign-in attempts. Please try again later.")
		return
	}

	// IncludingDeleted lets us spot soft-deleted users so we can restore
	// them on login during the grace window. Past the window they look
	// indistinguishable from "doesn't exist" — same response, same timing.
	user, err := h.q.GetUserByUsernameIncludingDeleted(r.Context(), h.d.Pool, username)
	if err != nil {
		// User doesn't exist — still hash to keep timing constant.
		password.VerifyAgainstDummy(pw)
		render("Incorrect username or password.")
		return
	}
	if user.DeletedAt.Valid && time.Since(user.DeletedAt.Time) >= deletionGraceWindow {
		password.VerifyAgainstDummy(pw)
		render("Incorrect username or password.")
		return
	}

	ok, err := password.Verify(pw, user.PasswordHash)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "login: verify", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !ok {
		render("Incorrect username or password.")
		return
	}

	// Restore-on-login: a within-grace soft-deleted user gets undeleted
	// the moment they prove ownership of the password. Best-effort: a
	// failed restore doesn't block the login (the row stays
	// soft-deleted; UI will surface the issue).
	if user.DeletedAt.Valid {
		if err := h.q.RestoreUserAccount(r.Context(), h.d.Pool, user.ID); err != nil {
			h.d.Logger.WarnContext(r.Context(), "login: restore", "error", err)
		} else {
			user.DeletedAt.Valid = false
		}
	}
	if user.SuspendedAt.Valid {
		render("This account has been suspended.")
		return
	}
	if h.d.RequireEmailVerification && !user.EmailVerified {
		render("Please verify your email before signing in. Check your inbox or request a new link.")
		return
	}

	// Forgive prior failed-attempt counter on success.
	_ = h.d.Limiter.Reset(r.Context(), h.d.Pool, "login", throttleKey)

	// If 2FA is enrolled and confirmed, redirect to the challenge step.
	// Pre-2FA marker carries user_id intent without granting full session.
	if t, terr := h.q.GetUserTOTP(r.Context(), h.d.Pool, user.ID); terr == nil && t.ConfirmedAt.Valid {
		s := middleware.SessionFromContext(r.Context())
		s.UserID = 0
		s.Pre2FAUserID = user.ID
		s.IssuedAt = time.Now().Unix()
		if err := h.d.SessionStore.Save(w, r, s); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "login: save pre-2fa session", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		dest := "/login/2fa"
		if next != "" && strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
			dest = "/login/2fa?next=" + url.QueryEscape(next)
		}
		//nolint:gosec // G710: dest is whitelisted to /login/2fa with sanitized next.
		http.Redirect(w, r, dest, http.StatusSeeOther)
		return
	}

	if err := h.q.TouchUserLastLogin(r.Context(), h.d.Pool, user.ID); err != nil {
		h.d.Logger.WarnContext(r.Context(), "login: touch last_login_at", "error", err)
	}

	// Session-fixation defense: bind user_id and re-issue cookie. The
	// AEAD store re-encrypts on every Save, producing a fresh ciphertext.
	// Epoch snapshotting is what powers "log out everywhere": this cookie
	// is invalidated the moment users.session_epoch advances past it.
	s := middleware.SessionFromContext(r.Context())
	s.UserID = user.ID
	s.Pre2FAUserID = 0
	s.Epoch = user.SessionEpoch
	s.IssuedAt = time.Now().Unix()
	if err := h.d.SessionStore.Save(w, r, s); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "login: save session", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	dest := "/"
	if next != "" && strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		// dest is constrained to single-leading-slash relative paths above,
		// which prevents the protocol-relative ("//evil.com") form gosec warns about.
		dest = next
	}
	//nolint:gosec // G710: dest is whitelisted to single-leading-slash relative paths.
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ---------------------------- logout -----------------------------------

func (h *Handlers) logoutSubmit(w http.ResponseWriter, r *http.Request) {
	h.d.SessionStore.Clear(w)
	http.Redirect(w, r, "/login?notice=logged-out", http.StatusSeeOther)
}

// ------------------------- password reset ------------------------------

func (h *Handlers) resetRequestForm(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, "auth/reset_request", map[string]any{
		"Title":     "Reset password",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Sent":      false,
	})
}

func (h *Handlers) resetRequestSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	addr := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))

	// Always show the same notice — no enumeration via this flow.
	notice := "If an account is registered to that address, we've sent a password-reset link."
	render := func() {
		h.renderPage(w, r, "auth/reset_request", map[string]any{
			"Title":     "Reset password",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Notice":    notice,
			"Sent":      true,
		})
	}

	if !looksLikeEmail(addr) {
		render()
		return
	}

	if err := h.d.Limiter.Hit(r.Context(), h.d.Pool, throttle.Limit{
		Scope: "reset", Identifier: "email:" + addr,
		Max: 3, Window: time.Hour,
	}); err != nil {
		h.writeRetryAfter(w, err)
		render()
		return
	}

	em, err := h.q.GetUserEmailByAddress(r.Context(), h.d.Pool, addr)
	if err != nil {
		// Don't even hint at existence.
		render()
		return
	}

	tokEnc, tokHash, err := token.New()
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "reset: token", "error", err)
		render()
		return
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}
	if _, err := h.q.CreatePasswordReset(r.Context(), h.d.Pool, usersdb.CreatePasswordResetParams{
		UserID:    em.UserID,
		TokenHash: tokHash,
		ExpiresAt: expires,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "reset: insert", "error", err)
		render()
		return
	}

	msg, err := email.ResetMessage(h.d.Branding, addr, tokEnc)
	if err == nil {
		if err := h.d.Email.Send(r.Context(), msg); err != nil {
			h.d.Logger.WarnContext(r.Context(), "reset: send", "error", err)
		}
	}
	render()
}

func (h *Handlers) resetConfirmForm(w http.ResponseWriter, r *http.Request) {
	tokStr := chi.URLParam(r, "token")
	if _, err := h.lookupValidReset(r.Context(), tokStr); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "reset link invalid or expired")
		return
	}
	h.renderPage(w, r, "auth/reset_confirm", map[string]any{
		"Title":     "Choose new password",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Token":     tokStr,
	})
}

func (h *Handlers) resetConfirmSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	tokStr := chi.URLParam(r, "token")
	pw := r.PostFormValue("password")

	render := func(msg string) {
		h.renderPage(w, r, "auth/reset_confirm", map[string]any{
			"Title":     "Choose new password",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Token":     tokStr,
			"Error":     msg,
		})
	}

	row, err := h.lookupValidReset(r.Context(), tokStr)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "reset link invalid or expired")
		return
	}
	if len(pw) < 10 {
		render("Password must be at least 10 characters.")
		return
	}
	if passwords.IsCommon(pw) {
		render("That password is too common. Please choose another.")
		return
	}

	hash, err := hashPassword(pw, h.d.Argon2)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "reset: hash", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.UpdateUserPassword(r.Context(), tx, usersdb.UpdateUserPasswordParams{
		ID:           row.UserID,
		PasswordHash: hash,
		PasswordAlgo: password.Algo,
	}); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.q.ConsumePasswordReset(r.Context(), tx, row.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	http.Redirect(w, r, "/login?notice=password-reset", http.StatusSeeOther)
}

// ------------------------- email verification --------------------------

func (h *Handlers) verifyEmail(w http.ResponseWriter, r *http.Request) {
	tokStr := chi.URLParam(r, "token")
	hash, err := token.HashOf(tokStr)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "verification link invalid")
		return
	}
	row, err := h.q.GetEmailVerificationByTokenHash(r.Context(), h.d.Pool, hash)
	if err != nil || row.UsedAt.Valid || time.Now().After(row.ExpiresAt.Time) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "verification link invalid or expired")
		return
	}
	em, err := h.q.GetUserEmailByID(r.Context(), h.d.Pool, row.UserEmailID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "verification link invalid")
		return
	}

	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()

	if err := h.q.MarkUserEmailVerified(r.Context(), tx, em.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if em.IsPrimary {
		if err := h.q.MarkUserEmailPrimaryVerified(r.Context(), tx, em.UserID); err != nil {
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}
	if err := h.q.ConsumeEmailVerification(r.Context(), tx, row.ID); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	http.Redirect(w, r, "/login?notice=verified", http.StatusSeeOther)
}

func (h *Handlers) verifyResendForm(w http.ResponseWriter, r *http.Request) {
	h.renderPage(w, r, "auth/verify_resend", map[string]any{
		"Title":     "Resend verification",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
	})
}

func (h *Handlers) verifyResendSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	addr := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	notice := "If a pending account is registered to that address, we've sent a fresh verification link."
	render := func() {
		h.renderPage(w, r, "auth/verify_resend", map[string]any{
			"Title":     "Resend verification",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Notice":    notice,
		})
	}
	if !looksLikeEmail(addr) {
		render()
		return
	}
	em, err := h.q.GetUserEmailByAddress(r.Context(), h.d.Pool, addr)
	if err != nil || em.Verified {
		render()
		return
	}
	tokEnc, tokHash, err := token.New()
	if err != nil {
		render()
		return
	}
	expires := pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		render()
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if err := h.q.SetVerificationToken(r.Context(), tx, usersdb.SetVerificationTokenParams{
		ID:                    em.ID,
		VerificationTokenHash: tokHash,
	}); err != nil {
		render()
		return
	}
	if _, err := h.q.CreateEmailVerification(r.Context(), tx, usersdb.CreateEmailVerificationParams{
		UserEmailID: em.ID, TokenHash: tokHash, ExpiresAt: expires,
	}); err != nil {
		render()
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		render()
		return
	}

	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, em.UserID)
	if err == nil {
		msg, err := email.VerifyMessage(h.d.Branding, addr, user.Username, tokEnc)
		if err == nil {
			_ = h.d.Email.Send(r.Context(), msg)
		}
	}
	render()
}

// ----------------------------- helpers ---------------------------------

func (h *Handlers) renderPage(w http.ResponseWriter, r *http.Request, page string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.d.Render.RenderPage(w, r, page, data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "render", "page", page, "error", err)
	}
}

func (h *Handlers) throttleSignup(r *http.Request) error {
	if err := h.d.Limiter.Hit(r.Context(), h.d.Pool, throttle.Limit{
		Scope: "signup", Identifier: "ip:" + clientIP(r),
		Max: 5, Window: time.Hour,
	}); err != nil {
		return err
	}
	return nil
}

func (h *Handlers) writeRetryAfter(w http.ResponseWriter, err error) {
	var t *throttle.ErrThrottled
	if errors.As(err, &t) {
		w.Header().Set("Retry-After", strconv.Itoa(int(t.RetryAfter.Seconds())))
		w.WriteHeader(http.StatusTooManyRequests)
	}
}

func (h *Handlers) lookupValidReset(ctx context.Context, encoded string) (usersdb.PasswordReset, error) {
	hash, err := token.HashOf(encoded)
	if err != nil {
		return usersdb.PasswordReset{}, err
	}
	row, err := h.q.GetPasswordResetByTokenHash(ctx, h.d.Pool, hash)
	if err != nil {
		return usersdb.PasswordReset{}, err
	}
	if row.UsedAt.Valid || time.Now().After(row.ExpiresAt.Time) {
		return usersdb.PasswordReset{}, errors.New("token expired or used")
	}
	return row, nil
}

// validateUsername returns a user-facing error string (suitable for
// rendering in the form's flash slot) or "" when the name is acceptable.
// Note: returns string, not error, because callers display these to the
// end user — staticcheck's ST1005 rule for error capitalization doesn't
// apply to UI copy.
func validateUsername(name string) string {
	if name == "" {
		return "Username is required."
	}
	if len(name) > 39 {
		return "Username may be at most 39 characters."
	}
	if !usernameRE.MatchString(name) {
		return "Username may contain only lowercase letters, digits, and hyphens, and cannot start or end with a hyphen."
	}
	if authpkg.IsReserved(name) {
		return "That username is reserved. Please choose another."
	}
	return ""
}

func looksLikeEmail(s string) bool {
	if len(s) < 3 || len(s) > 254 {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	if strings.IndexByte(s, ' ') >= 0 {
		return false
	}
	return true
}

// hashPassword wraps password.Hash so callers don't need to import the
// stdlib package alongside the local one.
func hashPassword(pw string, p password.Params) (string, error) {
	return password.Hash(pw, p)
}

func clientIP(r *http.Request) string {
	if ip := middleware.RealIPFromContext(r.Context(), r); ip != "" {
		return ip
	}
	if h, _, err := splitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return addr, "", nil
	}
	return addr[:i], addr[i+1:], nil
}

// isUniqueViolation matches Postgres SQLSTATE 23505. We use an interface
// shim so the auth package doesn't import pgconn directly.
func isUniqueViolation(err error) bool {
	type sqlStater interface {
		SQLState() string
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	for cur := err; cur != nil; cur = errors.Unwrap(cur) {
		if s, ok := cur.(sqlStater); ok && s.SQLState() == "23505" {
			return true
		}
	}
	return false
}
