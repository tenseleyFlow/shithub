// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"unicode/utf8"

	orgdomain "github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

const (
	orgProfileMaxDisplayName = 100
	orgProfileMaxLocation    = 80
	orgProfileMaxWebsite     = 200
)

type settingsProfileForm struct {
	DisplayName           string
	Description           string
	Website               string
	Location              string
	BillingEmail          string
	AllowMemberRepoCreate bool
}

func settingsProfileFormFromOrg(org orgsdb.Org) settingsProfileForm {
	return settingsProfileForm{
		DisplayName:           org.DisplayName,
		Description:           org.Description,
		Website:               org.Website,
		Location:              org.Location,
		BillingEmail:          org.BillingEmail,
		AllowMemberRepoCreate: org.AllowMemberRepoCreate,
	}
}

func (h *Handlers) settingsProfileSubmit(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}

	form := settingsProfileForm{
		DisplayName:           strings.TrimSpace(r.PostFormValue("display_name")),
		Description:           strings.TrimRight(r.PostFormValue("description"), " \t\r\n"),
		Website:               strings.TrimSpace(r.PostFormValue("website")),
		Location:              strings.TrimSpace(r.PostFormValue("location")),
		BillingEmail:          strings.TrimSpace(r.PostFormValue("billing_email")),
		AllowMemberRepoCreate: r.PostFormValue("allow_member_repo_create") == "on",
	}
	if form.DisplayName == "" {
		form.DisplayName = org.Slug
	}
	if msg := validateOrgProfile(&form); msg != "" {
		h.renderSettingsProfileWithForm(w, r, org, form, msg, "")
		return
	}

	q := orgsdb.New()
	tx, err := h.d.Pool.Begin(r.Context())
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org settings: begin update", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(r.Context())
		}
	}()
	if err := q.UpdateOrgProfile(r.Context(), tx, orgsdb.UpdateOrgProfileParams{
		ID:           org.ID,
		DisplayName:  form.DisplayName,
		Description:  form.Description,
		Location:     form.Location,
		Website:      form.Website,
		BillingEmail: form.BillingEmail,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org settings: update profile", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := q.SetOrgAllowMemberRepoCreate(r.Context(), tx, orgsdb.SetOrgAllowMemberRepoCreateParams{
		ID:                    org.ID,
		AllowMemberRepoCreate: form.AllowMemberRepoCreate,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org settings: update member repo create", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org settings: commit update", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	committed = true
	updated, err := q.GetOrgByID(r.Context(), h.d.Pool, org.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org settings: reload profile", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	h.renderSettingsProfile(w, r, updated, "", "Organization profile updated.")
}

func (h *Handlers) settingsDelete(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	if !strings.EqualFold(strings.TrimSpace(r.PostFormValue("confirm_slug")), org.Slug) {
		h.renderSettingsProfile(w, r, org, "Enter this organization's name to confirm deletion.", "")
		return
	}
	if err := orgdomain.SoftDelete(r.Context(), h.deps(), org.ID, viewer.ID); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org settings: soft delete", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	http.Redirect(w, r, "/settings/organizations", http.StatusSeeOther)
}

func (h *Handlers) renderSettingsProfileWithForm(
	w http.ResponseWriter,
	r *http.Request,
	org orgsdb.Org,
	form settingsProfileForm,
	errMsg string,
	success string,
) {
	_ = h.d.Render.RenderPage(w, r, "orgs/settings_profile", map[string]any{
		"Title":               org.Slug + " - profile settings",
		"CSRFToken":           middleware.CSRFTokenForRequest(r),
		"Org":                 org,
		"Form":                form,
		"AvatarURL":           "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":        "settings",
		"OrgSettingsActive":   "profile",
		"BillingEnabled":      h.d.BillingEnabled,
		"AvatarUploadEnabled": h.d.ObjectStore != nil,
		"HasAvatar":           org.AvatarObjectKey.Valid && org.AvatarObjectKey.String != "",
		"Error":               errMsg,
		"Success":             success,
	})
}

func validateOrgProfile(f *settingsProfileForm) string {
	if utf8.RuneCountInString(f.DisplayName) > orgProfileMaxDisplayName {
		return "Organization display name is too long."
	}
	if utf8.RuneCountInString(f.Description) > 350 {
		return "Description is too long (max 350 characters)."
	}
	if utf8.RuneCountInString(f.Location) > orgProfileMaxLocation {
		return "Location is too long."
	}
	if utf8.RuneCountInString(f.Website) > orgProfileMaxWebsite {
		return "Website URL is too long."
	}
	if strings.ContainsAny(f.DisplayName, "\r\n") || strings.ContainsAny(f.Location, "\r\n") {
		return "Single-line fields cannot contain newlines."
	}
	if f.Website != "" {
		if !strings.Contains(f.Website, "://") {
			f.Website = "https://" + f.Website
		}
		u, err := url.Parse(f.Website)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "Website must be an http(s) URL."
		}
	}
	if f.BillingEmail != "" {
		addr, err := mail.ParseAddress(f.BillingEmail)
		if err != nil || addr.Address != f.BillingEmail {
			return "Billing email must be a single valid email address."
		}
	}
	return ""
}
