// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

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
}

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

	h.renderProfileForm(w, r, form, "", "Profile updated.")
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
	return ""
}
