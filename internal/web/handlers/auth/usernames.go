// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// settingsUsernamesForm renders GET /settings/usernames. The page is
// reachable for Free users; the inputs are pro-locked there. Pro users
// see the form enabled with their current reservations listed.
func (h *Handlers) settingsUsernamesForm(w http.ResponseWriter, r *http.Request) {
	h.renderUsernamesForm(w, r, "", "")
}

// settingsUsernameReserve handles POST /settings/usernames. Adds a
// reservation. Free users get a friendly 4xx with the upgrade prompt;
// Pro users with the reservation cap reached get the same banner. The
// gate is also enforced via the entitlements decision so a third-party
// client cannot bypass the page-rendered Pro-lock.
func (h *Handlers) settingsUsernameReserve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	handle := strings.ToLower(strings.TrimSpace(r.PostFormValue("handle")))

	if msg := validateUsername(handle); msg != "" {
		h.renderUsernamesForm(w, r, msg, "")
		return
	}
	if msg, err := h.usernameReservationAvailable(r.Context(), handle, user.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/usernames: availability check", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	} else if msg != "" {
		h.renderUsernamesForm(w, r, msg, "")
		return
	}

	// Entitlement gate. Free users get an upgrade-banner-style message
	// rather than a generic 4xx so the path to /settings/billing is
	// one click away.
	allowed, decision, err := h.userReservationsAllowed(r.Context(), user.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings/usernames: entitlement check", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if !allowed {
		banner := decision.PrincipalUpgradeBanner("Username reservations", billing.PrincipalForUser(user.ID), "")
		h.renderUsernamesForm(w, r, banner.Message, "")
		return
	}

	// Reservation cap (Pro = 3).
	count, err := h.q.CountUsernameReservationsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/usernames: count reservations", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if count >= entitlements.ProUsernameReservationsCap {
		h.renderUsernamesForm(w, r, "You've reached the maximum of "+strconv.FormatInt(entitlements.ProUsernameReservationsCap, 10)+" reservations. Release one to add another.", "")
		return
	}

	if _, err := h.q.InsertUsernameReservation(r.Context(), h.d.Pool, usersdb.InsertUsernameReservationParams{
		UserID:         user.ID,
		ReservedHandle: handle,
	}); err != nil {
		if isUniqueViolation(err) {
			h.renderUsernamesForm(w, r, "That handle is already reserved.", "")
			return
		}
		h.d.Logger.ErrorContext(r.Context(), "settings/usernames: insert", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderUsernamesForm(w, r, "", "Reserved "+handle+".")
}

// settingsUsernameRelease handles POST /settings/usernames/{id}/release.
// The DELETE is scoped by user_id so id-guessing across users is a
// no-op rather than a privilege escalation.
func (h *Handlers) settingsUsernameRelease(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	id, err := strconv.ParseInt(r.PostFormValue("reservation_id"), 10, 64)
	if err != nil || id <= 0 {
		h.renderUsernamesForm(w, r, "Invalid reservation id.", "")
		return
	}
	if err := h.q.DeleteUsernameReservation(r.Context(), h.d.Pool, usersdb.DeleteUsernameReservationParams{
		ID:     id,
		UserID: user.ID,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/usernames: delete", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderUsernamesForm(w, r, "", "Reservation released.")
}

// renderUsernamesForm is the shared render path. Surfaces the user's
// current reservation list + entitlement state so the template can
// render the form in the appropriate disabled / enabled state.
func (h *Handlers) renderUsernamesForm(w http.ResponseWriter, r *http.Request, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := h.q.ListUsernameReservationsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings/usernames: list", "user_id", user.ID, "error", err)
	}
	allowed, _, _ := h.userReservationsAllowed(r.Context(), user.ID)
	h.renderPage(w, r, "settings/usernames", map[string]any{
		"Title":             "Username reservations",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"SettingsActive":    "usernames",
		"Reservations":      rows,
		"Cap":               entitlements.ProUsernameReservationsCap,
		"Used":              int64(len(rows)),
		"Remaining":         entitlements.ProUsernameReservationsCap - int64(len(rows)),
		"ReservationsAllow": allowed,
		"Error":             errMsg,
		"Success":           successMsg,
	})
}

// userReservationsAllowed checks the FeatureUsernameReservations gate.
// Returns (allowed, decision, error). The decision is returned so the
// caller can surface the upgrade banner copy when allowed=false.
//
// Honors the BillingEnforce.UserUsernameReservations soak path: with
// the flag off and the entitlement denying, the would-deny is logged
// but allowed=true is returned so the write lands during soak. Added
// in PRO-EXT_SR2-09 — prior to that the function always hard-denied
// without an operator knob, contradicting the report-only contract
// the runbook claimed governed the feature.
func (h *Handlers) userReservationsAllowed(ctx context.Context, userID int64) (bool, entitlements.Decision, error) {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureUsernameReservations)
	if err != nil {
		return false, entitlements.Decision{}, err
	}
	if decision.Allowed {
		return true, decision, nil
	}
	mode := "report_only"
	if h.d.BillingEnforce.UserUsernameReservations {
		mode = "enforce"
	}
	h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
		"principal", billing.PrincipalForUser(userID).String(),
		"principal_kind", string(billing.SubjectKindUser),
		"principal_id", userID,
		"feature", string(entitlements.FeatureUsernameReservations),
		"reason", string(decision.Reason),
		"required_plan", string(decision.RequiredPlan),
		"mode", mode,
		"surface", "settings-username-reservations")
	return !h.d.BillingEnforce.UserUsernameReservations, decision, nil
}

// usernameReservationAvailable checks whether `handle` is free for
// `userID` to reserve. Returns (message, err): message is a user-
// facing reason the handle is blocked (empty when available); err is
// a real DB failure that should 500.
//
// Checks the four existing squat-protection surfaces in order:
// hardcoded reserved list, active user, redirected old-name, and
// another user's reservation. The `userID` excludes that user's own
// reservation from the last check so a user converting one of their
// reservations to an active handle is not blocked by it.
func (h *Handlers) usernameReservationAvailable(ctx context.Context, handle string, userID int64) (string, error) {
	if authpkg.IsReserved(handle) {
		return "That handle is reserved by the system", nil
	}
	if _, err := h.q.GetUserByUsername(ctx, h.d.Pool, handle); err == nil {
		return "That handle is in use by another account", nil
	}
	if _, err := h.q.LookupUsernameRedirect(ctx, h.d.Pool, handle); err == nil {
		return "That handle was previously used and remains protected as a redirect", nil
	}
	reserved, err := h.q.IsUsernameReservedByAnother(ctx, h.d.Pool, usersdb.IsUsernameReservedByAnotherParams{
		ReservedHandle: handle,
		ExceptUserID:   userID,
	})
	if err != nil {
		return "", err
	}
	if reserved {
		return "That handle is reserved by another account", nil
	}
	return "", nil
}
