// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// mountUserEmails registers the email-listing endpoint. Mutations (add /
// delete) keep using the HTML settings surface for now; the REST surface
// is read-only until a future batch grows verify/send-verification flows.
//
//	GET /api/v1/user/emails — list emails for the authenticated user.
//
// Optional ?verified=true|false filter mirrors GitHub's /user/emails
// shape (which only returns verified by default; we expose both flavors
// behind an explicit query param so callers can audit unverified
// addresses too).
func (h *Handlers) mountUserEmails(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireScope(pat.ScopeUserRead))
		r.Get("/api/v1/user/emails", h.userEmailsList)
	})
}

type userEmailResponse struct {
	ID       int64  `json:"id"`
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

func (h *Handlers) userEmailsList(w http.ResponseWriter, r *http.Request) {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}

	rows, err := h.q.ListUserEmailsForUser(r.Context(), h.d.Pool, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: list user emails", "error", err)
		writeAPIError(w, http.StatusInternalServerError, "list failed")
		return
	}

	filter, hasFilter := emailVerifiedFilter(r)
	out := make([]userEmailResponse, 0, len(rows))
	for _, em := range rows {
		if hasFilter && em.Verified != filter {
			continue
		}
		out = append(out, userEmailResponse{
			ID:       em.ID,
			Email:    string(em.Email),
			Primary:  em.IsPrimary,
			Verified: em.Verified,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// emailVerifiedFilter parses the ?verified= query param. Returns
// (value, true) when explicitly set to "true" or "false"; (false, false)
// when omitted or unparseable (treated as "no filter").
func emailVerifiedFilter(r *http.Request) (bool, bool) {
	switch r.URL.Query().Get("verified") {
	case "true":
		return true, true
	case "false":
		return false, true
	default:
		return false, false
	}
}
