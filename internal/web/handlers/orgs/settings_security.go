// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type orgSecuritySettingsView struct {
	RequireTwoFactor bool
}

func (h *Handlers) settingsSecurity(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	settings, ok := h.loadOrgSecuritySettings(w, r, org.ID)
	if !ok {
		return
	}
	h.renderOrgSecuritySettings(w, r, org, settings, "", orgSecuritySettingsNotice(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsSecuritySubmit(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	settings := orgSecuritySettingsView{
		RequireTwoFactor: r.PostFormValue("require_two_factor") == "on",
	}
	if _, err := orgsdb.New().UpsertOrgSecuritySettings(r.Context(), h.d.Pool, orgsdb.UpsertOrgSecuritySettingsParams{
		OrgID:            org.ID,
		RequireTwoFactor: settings.RequireTwoFactor,
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security settings: save", "org_id", org.ID, "error", err)
		h.renderOrgSecuritySettings(w, r, org, settings, "Could not save organization security settings.", "")
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	actor, meta := viewer.AuditActor(map[string]any{
		"require_two_factor": settings.RequireTwoFactor,
	})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, audit.ActionOrgRequired2FAUpdated, audit.TargetOrg, org.ID, meta)
	http.Redirect(w, r, orgSecuritySettingsPath(org.Slug)+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) loadOrgSecuritySettings(w http.ResponseWriter, r *http.Request, orgID int64) (orgSecuritySettingsView, bool) {
	row, err := orgsdb.New().GetOrgSecuritySettings(r.Context(), h.d.Pool, orgID)
	if err == nil {
		return orgSecuritySettingsView{RequireTwoFactor: row.RequireTwoFactor}, true
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return orgSecuritySettingsView{}, true
	}
	h.d.Logger.ErrorContext(r.Context(), "org security settings: load", "org_id", orgID, "error", err)
	h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
	return orgSecuritySettingsView{}, false
}

func (h *Handlers) renderOrgSecuritySettings(w http.ResponseWriter, r *http.Request, org orgsdb.Org, settings orgSecuritySettingsView, errMsg, notice string) {
	data := map[string]any{
		"Title":             org.Slug + " · Security settings",
		"Org":               org,
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"OrgSettingsActive": "security",
		"BillingEnabled":    h.billingConfigured(),
		"Error":             errMsg,
		"Notice":            notice,
		"Settings":          settings,
	}
	h.d.Render.RenderPage(w, r, "orgs/settings_security", data)
}

func orgSecuritySettingsNotice(code string) string {
	if code == "saved" {
		return "Organization security settings updated."
	}
	return ""
}

func orgSecuritySettingsPath(slug string) string {
	return "/organizations/" + slug + "/settings/security"
}
