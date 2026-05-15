// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-07a — saved replies CRUD. Free users get
// entitlements.FreeSavedRepliesCap entries; Pro users get up to
// entitlements.ProSavedRepliesCap (effectively unlimited).
//
// Gate posture:
//   - Free user under the cap: reply creation succeeds for both report-
//     only and enforce modes (the cap *is* the gate).
//   - Free user at the cap, report-only: insert proceeds; the would-deny
//     is logged so operators can watch the soak.
//   - Free user at the cap, enforce: insert is rejected with the
//     upgrade banner. UI surfaces the locked "Add another reply" button
//     for the same scenario.

// savedReplyMaxBodyChars / NameChars mirror the migration check
// constraints so we surface a friendly message before the DB does.
const (
	savedReplyMaxNameChars = 80
	savedReplyMaxBodyChars = 8000
)

// settingsSavedRepliesForm renders GET /settings/saved-replies.
func (h *Handlers) settingsSavedRepliesForm(w http.ResponseWriter, r *http.Request) {
	h.renderSavedRepliesForm(w, r, "", "")
}

// settingsSavedReplyCreate handles POST /settings/saved-replies.
func (h *Handlers) settingsSavedReplyCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	body := strings.TrimSpace(r.PostFormValue("body"))

	if msg := validateSavedReply(name, body); msg != "" {
		h.renderSavedRepliesForm(w, r, msg, "")
		return
	}

	count, err := h.q.CountSavedRepliesForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/saved-replies: count", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	allowedUnlimited, decision, derr := h.userSavedRepliesUnlimitedAllowed(r.Context(), user.ID)
	if derr != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/saved-replies: entitlement check", "user_id", user.ID, "error", derr)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if allowedUnlimited {
		if count >= entitlements.ProSavedRepliesCap {
			h.renderSavedRepliesForm(w, r, "You've reached the maximum number of saved replies. Delete one to add another.", "")
			return
		}
	} else {
		// Free user.
		if count >= entitlements.FreeSavedRepliesCap {
			if h.d.BillingEnforce.UserSavedRepliesUnlimited {
				banner := decision.PrincipalUpgradeBanner("Unlimited saved replies", billing.PrincipalForUser(user.ID), "")
				h.renderSavedRepliesForm(w, r, banner.Message, "")
				return
			}
			// Report-only: would-deny was already logged by
			// userSavedRepliesUnlimitedAllowed. Let the insert proceed
			// up to the Pro sanity ceiling so users don't strand data,
			// but cap at ProSavedRepliesCap for DB safety.
			if count >= entitlements.ProSavedRepliesCap {
				h.renderSavedRepliesForm(w, r, "You've reached the maximum number of saved replies. Delete one to add another.", "")
				return
			}
		}
	}

	if _, err := h.q.InsertSavedReply(r.Context(), h.d.Pool, usersdb.InsertSavedReplyParams{
		UserID: user.ID,
		Name:   name,
		Body:   body,
	}); err != nil {
		if isUniqueViolation(err) {
			h.renderSavedRepliesForm(w, r, "You already have a saved reply with that name.", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "settings/saved-replies: insert", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSavedRepliesForm(w, r, "", "Saved reply added.")
}

// settingsSavedReplyUpdate handles POST /settings/saved-replies/{id}/update.
func (h *Handlers) settingsSavedReplyUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSavedRepliesForm(w, r, "Invalid saved-reply id.", "")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	body := strings.TrimSpace(r.PostFormValue("body"))
	if msg := validateSavedReply(name, body); msg != "" {
		h.renderSavedRepliesForm(w, r, msg, "")
		return
	}
	if err := h.q.UpdateSavedReply(r.Context(), h.d.Pool, usersdb.UpdateSavedReplyParams{
		ID:     id,
		UserID: user.ID,
		Name:   name,
		Body:   body,
	}); err != nil {
		if isUniqueViolation(err) {
			h.renderSavedRepliesForm(w, r, "You already have a saved reply with that name.", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "settings/saved-replies: update", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSavedRepliesForm(w, r, "", "Saved reply updated.")
}

// settingsSavedReplyDelete handles POST /settings/saved-replies/{id}/delete.
func (h *Handlers) settingsSavedReplyDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderSavedRepliesForm(w, r, "Invalid saved-reply id.", "")
		return
	}
	if err := h.q.DeleteSavedReply(r.Context(), h.d.Pool, usersdb.DeleteSavedReplyParams{
		ID:     id,
		UserID: user.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/saved-replies: delete", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSavedRepliesForm(w, r, "", "Saved reply deleted.")
}

// renderSavedRepliesForm is the shared render path.
func (h *Handlers) renderSavedRepliesForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListSavedRepliesForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings/saved-replies: list", "user_id", user.ID, "error", err)
	}
	visibleCap, isFree := h.savedReplyVisibleCap(r.Context(), user.ID)
	used := int64(len(rows))
	h.renderPage(w, r, "settings/saved_replies", map[string]any{
		"Title":          "Saved replies",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "saved-replies",
		"Replies":        rows,
		"Cap":            visibleCap,
		"Used":           used,
		"Remaining":      visibleCap - used,
		"FreeCap":        entitlements.FreeSavedRepliesCap,
		// AtFreeCap is the visible-but-locked signal for the template.
		// Drives the disabled "Add another" button + the upgrade CTA
		// inline. We fire it whenever a Free user has reached the Free
		// cap — even in report-only mode — so the affordance is
		// consistent with the campaign's "visible-but-locked UI for Free
		// users" rule. The actual enforcement decision (block vs allow)
		// is made in settingsSavedReplyCreate against the enforce flag.
		"AtFreeCap":    isFree && used >= entitlements.FreeSavedRepliesCap,
		"Error":        errMsg,
		"Success":      successMsg,
		"MaxNameChars": savedReplyMaxNameChars,
		"MaxBodyChars": savedReplyMaxBodyChars,
	})
}

// savedReplyVisibleCap returns the cap surfaced in the UI plus whether
// the principal is Free. Free users always see FreeSavedRepliesCap so
// the locked-UI affordance is consistent across report-only and
// enforce modes; Pro users see ProSavedRepliesCap (the DB sanity
// ceiling, advertised as "unlimited").
func (h *Handlers) savedReplyVisibleCap(ctx context.Context, userID int64) (int64, bool) {
	decision, err := entitlements.CheckPrincipalFeature(ctx, entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureSavedRepliesUnlimited)
	if err != nil || !decision.Allowed {
		return entitlements.FreeSavedRepliesCap, true
	}
	return entitlements.ProSavedRepliesCap, false
}

// userSavedRepliesUnlimitedAllowed wraps CheckPrincipalFeature and logs
// would-denies for telemetry. Mirrors userReservationsAllowed.
func (h *Handlers) userSavedRepliesUnlimitedAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureSavedRepliesUnlimited)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if !decision.Allowed {
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureSavedRepliesUnlimited),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only")
	}
	return decision.Allowed, decision, nil
}

// validateSavedReply applies the same shape checks the DB enforces so
// the user gets a friendly message before the constraint violation.
func validateSavedReply(name, body string) string {
	if name == "" {
		return "Name is required."
	}
	if len([]rune(name)) > savedReplyMaxNameChars {
		return "Name is too long (max 80 characters)."
	}
	if body == "" {
		return "Body is required."
	}
	if len(body) > savedReplyMaxBodyChars {
		return "Body is too long (max 8000 characters)."
	}
	return ""
}
