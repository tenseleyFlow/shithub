// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tenseleyFlow/shithub/internal/auth/devicecode"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// deviceCodeForm renders the user-facing verification page. Three
// shapes:
//   - no user_code → show the entry form.
//   - user_code present + recognised + pending → show the approve/deny form.
//   - user_code present but already terminal (approved/denied/expired)
//     → render the "you can close this window" terminal page so the
//     polling device sees the result on its next exchange.
//
// Unauthenticated callers are redirected to /login with a `next=` that
// preserves the user_code so the flow resumes after sign-in.
func (h *Handlers) deviceCodeForm(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.IsAnonymous() {
		next := "/login/device"
		if uc := strings.TrimSpace(r.URL.Query().Get("user_code")); uc != "" {
			next += "?user_code=" + url.QueryEscape(uc)
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
		return
	}
	userCode := strings.TrimSpace(r.URL.Query().Get("user_code"))
	data := map[string]any{
		"Title":     "Authorize device",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"UserCode":  userCode,
	}
	if userCode == "" {
		h.renderPage(w, r, "auth/device_code", data)
		return
	}
	row, err := devicecode.LookupByUserCode(r.Context(), devicecode.Deps{Pool: h.d.Pool}, userCode)
	if err != nil {
		data["Error"] = "We don't recognise that code. Check the value and try again."
		h.renderPage(w, r, "auth/device_code", data)
		return
	}
	switch {
	case row.ApprovedAt.Valid:
		data["Terminal"] = true
		data["Notice"] = "Already authorized."
	case row.DeniedAt.Valid:
		data["Terminal"] = true
		data["Notice"] = "Already denied."
	case time.Now().After(row.ExpiresAt.Time):
		data["Terminal"] = true
		data["Notice"] = "This code expired. Ask your device for a new one."
	default:
		data["Approval"] = true
		data["ClientID"] = row.ClientID
		data["Scopes"] = row.Scopes
		data["UserCode"] = row.UserCode
	}
	h.renderPage(w, r, "auth/device_code", data)
}

// deviceCodeSubmit handles the approve / deny click. We re-resolve the
// row by user_code rather than trust the form id so a malicious page
// can't trick the user into approving a different grant.
func (h *Handlers) deviceCodeSubmit(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	if user.IsAnonymous() {
		http.Redirect(w, r, "/login?next="+url.QueryEscape("/login/device"), http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	userCode := strings.TrimSpace(r.PostForm.Get("user_code"))
	action := r.PostForm.Get("action")
	if userCode == "" {
		http.Redirect(w, r, "/login/device", http.StatusSeeOther)
		return
	}
	row, err := devicecode.LookupByUserCode(r.Context(), devicecode.Deps{Pool: h.d.Pool}, userCode)
	if err != nil {
		h.renderPage(w, r, "auth/device_code", map[string]any{
			"Title":     "Authorize device",
			"CSRFToken": middleware.CSRFTokenForRequest(r),
			"Error":     "We don't recognise that code.",
		})
		return
	}
	deps := devicecode.Deps{Pool: h.d.Pool, Audit: h.d.Audit}
	switch action {
	case "approve":
		err = devicecode.Approve(r.Context(), deps, row.ID, user.ID)
	case "deny":
		err = devicecode.Deny(r.Context(), deps, row.ID, user.ID)
	default:
		http.Redirect(w, r, "/login/device?user_code="+url.QueryEscape(userCode), http.StatusSeeOther)
		return
	}
	data := map[string]any{
		"Title":     "Authorize device",
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Terminal":  true,
	}
	switch {
	case err == nil && action == "approve":
		data["Notice"] = "Authorized. Return to your device."
	case err == nil && action == "deny":
		data["Notice"] = "Denied. Your device will see the result on its next poll."
	case errors.Is(err, devicecode.ErrExpiredToken):
		data["Error"] = "This code expired before you could authorize it."
	case errors.Is(err, devicecode.ErrAlreadyTerminal):
		data["Notice"] = "Already finalized."
	default:
		h.d.Logger.ErrorContext(r.Context(), "device-code submit", "error", err)
		data["Error"] = "Could not complete authorization. Please retry."
	}
	h.renderPage(w, r, "auth/device_code", data)
}
