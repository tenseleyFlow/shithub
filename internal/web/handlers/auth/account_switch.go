// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/session"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) accountSwitchSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}

	targetID, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("user_id")), 10, 64)
	if err != nil || targetID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "invalid account")
		return
	}

	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.ID == targetID {
		//nolint:gosec // G710: accountSwitchReturnPath only returns single-leading-slash relative paths.
		http.Redirect(w, r, accountSwitchReturnPath(r), http.StatusSeeOther)
		return
	}

	s := middleware.SessionFromContext(r.Context())
	remembered, ok := s.KnownAccount(targetID)
	if !ok {
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "account is not active in this browser")
		return
	}

	user, err := h.q.GetUserByID(r.Context(), h.d.Pool, targetID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.ForgetAccount(targetID)
			_ = h.d.SessionStore.Save(w, r, s)
			h.d.Render.HTTPError(w, r, http.StatusForbidden, "account is no longer available")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "account switch: load target", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if user.SuspendedAt.Valid {
		s.ForgetAccount(targetID)
		_ = h.d.SessionStore.Save(w, r, s)
		h.d.Render.HTTPError(w, r, http.StatusForbidden, "account is suspended")
		return
	}
	if remembered.Epoch != user.SessionEpoch {
		s.ForgetAccount(targetID)
		if err := h.d.SessionStore.Save(w, r, s); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "account switch: save expired account removal", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		http.Redirect(w, r, "/login?notice=account-expired", http.StatusSeeOther)
		return
	}

	h.rememberCurrentSessionAccount(r.Context(), s)
	s.UserID = user.ID
	s.Pre2FAUserID = 0
	s.Epoch = user.SessionEpoch
	s.Recent2FAAt = remembered.Recent2FAAt
	s.ImpersonatedUserID = 0
	s.ImpersonateWriteOK = false
	s.ImpersonationStartedAt = 0
	rememberUserAccount(s, user, s.Recent2FAAt)
	if err := h.d.SessionStore.Save(w, r, s); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "account switch: save session", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	//nolint:gosec // G710: accountSwitchReturnPath only returns single-leading-slash relative paths.
	http.Redirect(w, r, accountSwitchReturnPath(r), http.StatusSeeOther)
}

func (h *Handlers) rememberCurrentSessionAccount(ctx context.Context, s *session.Session) {
	if s == nil || s.UserID == 0 || s.ImpersonatedUserID != 0 {
		return
	}
	user, err := h.q.GetUserByID(ctx, h.d.Pool, s.UserID)
	if err != nil {
		return
	}
	if user.SuspendedAt.Valid || user.SessionEpoch != s.Epoch {
		return
	}
	rememberUserAccount(s, user, s.Recent2FAAt)
}

func rememberUserAccount(s *session.Session, user usersdb.User, recent2FAAt int64) {
	if s == nil || user.ID == 0 || user.Username == "" {
		return
	}
	s.RememberAccount(session.KnownAccount{
		UserID:       user.ID,
		Username:     user.Username,
		DisplayName:  user.DisplayName,
		Epoch:        user.SessionEpoch,
		Recent2FAAt:  recent2FAAt,
		RememberedAt: time.Now().Unix(),
	})
}

func accountSwitchReturnPath(r *http.Request) string {
	if p := strings.TrimSpace(r.PostFormValue("return_to")); isSafeRelativePath(p) {
		return p
	}
	if ref := r.Referer(); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			if !u.IsAbs() && isSafeRelativePath(u.RequestURI()) {
				return u.RequestURI()
			}
			if u.IsAbs() && u.Host == r.Host && isSafeRelativePath(u.RequestURI()) {
				return u.RequestURI()
			}
		}
	}
	return defaultPostLoginPath
}

func isSafeRelativePath(path string) bool {
	return path != "" && strings.HasPrefix(path, "/") && !strings.HasPrefix(path, "//")
}
