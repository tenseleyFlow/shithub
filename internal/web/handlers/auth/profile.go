// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// Field length caps. These mirror the CHECK constraints added in
// migration 0014; the handler validates before the DB does so we surface
// a friendly error instead of a 500 from a constraint violation.
const (
	maxDisplayName = 100
	maxBio         = 500
	maxLocation    = 80
	maxWebsite     = 200
	maxCompany     = 80
	maxPronouns    = 40
)

// profileForm is the editor's form state. We round-trip on validation
// errors so the user doesn't lose their input.
type profileForm struct {
	DisplayName string
	Bio         string
	Location    string
	Website     string
	Company     string
	Pronouns    string
	// PRO-EXT01-04: Pro-only vanity controls. Free users see these
	// inputs as pro-locked in the template; the handler still parses
	// them so a Pro user round-trips on validation errors and the
	// initial Free-page render carries the current (likely empty)
	// values for display.
	AccentHex string
	Layout    string
}

// accentHexRe matches the strict #rrggbb shape allowed by the
// users.profile_accent_hex CHECK constraint. The handler rejects
// anything else with a friendly message; this is also defense-in-depth
// against CSS injection — the value lands in a `style="--var: $hex"`
// attribute on the profile page.
var accentHexRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// profileLayoutKnown is the closed set the users.profile_layout CHECK
// constraint enforces. Adding a new layout means: extending this set,
// adding the CSS class branch in the template, and shipping a
// migration that widens the constraint.
var profileLayoutKnown = map[string]bool{"list": true, "featured": true}

// settingsProfileForm renders GET /settings/profile prefilled from the DB.
func (h *Handlers) settingsProfileForm(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	row, err := h.q.GetUserByID(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/profile: load", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderProfileForm(w, r, profileForm{
		DisplayName: row.DisplayName,
		Bio:         row.Bio,
		Location:    row.Location,
		Website:     row.Website,
		Company:     row.Company,
		Pronouns:    row.Pronouns,
		AccentHex:   row.ProfileAccentHex,
		Layout:      row.ProfileLayout,
	}, "", "")
}

// settingsProfileSubmit handles POST /settings/profile.
func (h *Handlers) settingsProfileSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	user := middleware.CurrentUserFromContext(r.Context())
	form := profileForm{
		DisplayName: strings.TrimSpace(r.PostFormValue("display_name")),
		Bio:         strings.TrimRight(r.PostFormValue("bio"), " \t\r\n"),
		Location:    strings.TrimSpace(r.PostFormValue("location")),
		Website:     strings.TrimSpace(r.PostFormValue("website")),
		Company:     strings.TrimSpace(r.PostFormValue("company")),
		Pronouns:    strings.TrimSpace(r.PostFormValue("pronouns")),
		AccentHex:   strings.ToLower(strings.TrimSpace(r.PostFormValue("accent_hex"))),
		Layout:      strings.TrimSpace(r.PostFormValue("profile_layout")),
	}
	if msg := validateProfile(&form); msg != "" {
		h.renderProfileForm(w, r, form, msg, "")
		return
	}

	if err := h.q.UpdateUserProfile(r.Context(), h.d.Pool, usersdb.UpdateUserProfileParams{
		ID:          user.ID,
		DisplayName: form.DisplayName,
		Bio:         form.Bio,
		Location:    form.Location,
		Website:     form.Website,
		Company:     form.Company,
		Pronouns:    form.Pronouns,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/profile: update", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	// PRO-EXT01-04: vanity write is gated. A Free user submitting the
	// form keeps the rest of the profile update but drops the vanity
	// payload silently — the inputs are pro-locked in the template, so
	// the only way to send non-default values is to bypass the UI. We
	// don't error on bypass to keep Free profile saves frictionless;
	// the values just don't persist.
	if h.userVanityAllowed(r, user.ID) {
		if err := h.q.UpdateUserProfileVanity(r.Context(), h.d.Pool, usersdb.UpdateUserProfileVanityParams{
			ID:               user.ID,
			ProfileAccentHex: form.AccentHex,
			ProfileLayout:    form.Layout,
		}); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "settings/profile: vanity update", "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
	}

	h.renderProfileForm(w, r, form, "", "Profile updated.")
}

// userVanityAllowed checks the FeatureProfileVanity gate. Returns
// false on any error (defensive — a degraded entitlements path can't
// silently promote a Free user). Logs the would-deny when a Free user
// attempts the write so PRO-EXT01-17's telemetry can attribute
// upgrade-driving traffic.
func (h *Handlers) userVanityAllowed(r *http.Request, userID int64) bool {
	decision, err := entitlements.CheckPrincipalFeature(r.Context(),
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeatureProfileVanity)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "settings/profile: vanity entitlement check", "user_id", userID, "error", err)
		return false
	}
	if !decision.Allowed {
		h.d.Logger.InfoContext(r.Context(), "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeatureProfileVanity),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", "report_only")
	}
	return decision.Allowed
}

// renderProfileForm is the shared render path. errMsg / successMsg are
// mutually exclusive in practice but we let the template show whichever is set.
func (h *Handlers) renderProfileForm(w http.ResponseWriter, r *http.Request, form profileForm, errMsg, successMsg string) {
	user := middleware.CurrentUserFromContext(r.Context())
	hasAvatar := false
	if row, err := h.q.GetUserByID(r.Context(), h.d.Pool, user.ID); err == nil {
		hasAvatar = row.AvatarObjectKey.Valid && row.AvatarObjectKey.String != ""
	}
	h.renderPage(w, r, "settings/profile", map[string]any{
		"Title":               "Public profile",
		"CSRFToken":           middleware.CSRFTokenForRequest(r),
		"SettingsActive":      "profile",
		"Username":            user.Username,
		"Form":                form,
		"Error":               errMsg,
		"Success":             successMsg,
		"AvatarUploadEnabled": h.d.ObjectStore != nil,
		"HasAvatar":           hasAvatar,
		"VanityAllowed":       h.userVanityAllowed(r, user.ID),
	})
}

// validateProfile enforces length caps and (if present) URL shape on
// the website. It returns a friendly message or "" on success.
func validateProfile(f *profileForm) string {
	if utf8.RuneCountInString(f.DisplayName) > maxDisplayName {
		return "Display name is too long."
	}
	if utf8.RuneCountInString(f.Bio) > maxBio {
		return "Bio is too long (max 500 characters)."
	}
	if utf8.RuneCountInString(f.Location) > maxLocation {
		return "Location is too long."
	}
	if utf8.RuneCountInString(f.Website) > maxWebsite {
		return "Website URL is too long."
	}
	if utf8.RuneCountInString(f.Company) > maxCompany {
		return "Company is too long."
	}
	if utf8.RuneCountInString(f.Pronouns) > maxPronouns {
		return "Pronouns is too long."
	}
	if f.Website != "" {
		// Auto-prefix bare hosts so users can paste "example.com".
		if !strings.Contains(f.Website, "://") {
			f.Website = "https://" + f.Website
		}
		u, err := url.Parse(f.Website)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "Website must be an http(s) URL."
		}
	}
	if strings.ContainsAny(f.DisplayName, "\r\n") ||
		strings.ContainsAny(f.Location, "\r\n") ||
		strings.ContainsAny(f.Company, "\r\n") ||
		strings.ContainsAny(f.Pronouns, "\r\n") {
		return "Single-line fields cannot contain newlines."
	}
	if f.AccentHex != "" && !accentHexRe.MatchString(f.AccentHex) {
		return "Accent color must be a #rrggbb hex value."
	}
	if f.Layout == "" {
		f.Layout = "list"
	}
	if !profileLayoutKnown[f.Layout] {
		return "Unknown profile layout."
	}
	return ""
}
