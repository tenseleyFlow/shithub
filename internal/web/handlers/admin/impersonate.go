// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// impersonateStart begins an impersonation session against the
// target user. Defaults to read-only. Records an audit row carrying
// both the real admin id and the target user id.
//
// Cross-tab guard: starting a new impersonation cancels any prior
// one (we just overwrite the session field). The audit row makes
// the transition traceable.
func (h *Handlers) impersonateStart(w http.ResponseWriter, r *http.Request) {
	target, ok := h.loadUser(w, r)
	if !ok {
		return
	}
	if target.IsSiteAdmin {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "can't impersonate another site admin")
		return
	}
	if target.DeletedAt.Valid {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "can't impersonate a deleted account")
		return
	}
	store := h.sessionStoreOrFail(w, r)
	if store == nil {
		return
	}
	s, err := store.Load(r)
	if err != nil || s == nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	s.ImpersonatedUserID = target.ID
	s.ImpersonateWriteOK = false
	s.ImpersonationStartedAt = time.Now().Unix()
	if err := store.Save(w, r, s); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.recordAdminAction(r, audit.ActionAdminImpersonateStarted, audit.TargetUser, target.ID,
		map[string]any{"target_username": target.Username})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// impersonateStop ends the active impersonation.
func (h *Handlers) impersonateStop(w http.ResponseWriter, r *http.Request) {
	store := h.sessionStoreOrFail(w, r)
	if store == nil {
		return
	}
	s, err := store.Load(r)
	if err != nil || s == nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	target := s.ImpersonatedUserID
	s.ImpersonatedUserID = 0
	s.ImpersonateWriteOK = false
	s.ImpersonationStartedAt = 0
	if err := store.Save(w, r, s); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if target != 0 {
		h.recordAdminAction(r, audit.ActionAdminImpersonateStopped, audit.TargetUser, target, nil)
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// impersonateEnableWrites flips ImpersonateWriteOK after a typed-name
// confirm. The admin types the target username; mismatch denies.
func (h *Handlers) impersonateEnableWrites(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	confirm := strings.TrimSpace(r.PostFormValue("confirm"))
	store := h.sessionStoreOrFail(w, r)
	if store == nil {
		return
	}
	s, err := store.Load(r)
	if err != nil || s == nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if s.ImpersonatedUserID == 0 {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "no active impersonation")
		return
	}
	target, err := h.uq.GetUserByID(r.Context(), h.d.Pool, s.ImpersonatedUserID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if confirm != target.Username {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "confirmation didn't match the target's username")
		return
	}
	s.ImpersonateWriteOK = true
	if err := store.Save(w, r, s); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.recordAdminAction(r, audit.ActionAdminImpersonateWriteOn, audit.TargetUser, target.ID, nil)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// sessionStoreOrFail extracts the store from request context. Writes
// 500 + returns nil on miss (the Save middleware should always have
// installed it).
func (h *Handlers) sessionStoreOrFail(w http.ResponseWriter, r *http.Request) session.Store {
	store := middleware.SessionStoreFromContext(r.Context())
	if store == nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return nil
	}
	return store
}
