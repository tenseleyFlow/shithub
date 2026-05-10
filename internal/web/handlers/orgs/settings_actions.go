// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsvars "github.com/tenseleyFlow/shithub/internal/actions/variables"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) settingsActionsSecrets(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderOrgActionsSettings(w, r, org, "secrets", "", orgActionsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsSecretSet(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if h.d.SecretBox == nil {
		http.Error(w, "actions secret key not configured", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := []byte(r.PostFormValue("value"))
	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Set(r.Context(), secrets.OrgScope(org.ID), name, value, viewer.ID)
	if err != nil {
		h.renderOrgActionsSettings(w, r, org, "secrets", friendlyOrgActionsSecretError(err), "")
		return
	}
	h.recordOrgActionsAudit(r, viewer, audit.ActionActionsSecretSet, org.ID, name)
	http.Redirect(w, r, orgActionsSettingsPath(org.Slug, "secrets")+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsSecretDelete(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Delete(r.Context(), secrets.OrgScope(org.ID), name)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidName) {
			http.Error(w, "bad secret name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordOrgActionsAudit(r, viewer, audit.ActionActionsSecretDeleted, org.ID, name)
	http.Redirect(w, r, orgActionsSettingsPath(org.Slug, "secrets")+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsVariables(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderOrgActionsSettings(w, r, org, "variables", "", orgActionsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsVariableSet(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "form parse", http.StatusBadRequest)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	name := strings.TrimSpace(r.PostFormValue("name"))
	value := r.PostFormValue("value")
	err := actionsvars.Deps{Pool: h.d.Pool}.Set(r.Context(), actionsvars.OrgScope(org.ID), name, value, viewer.ID)
	if err != nil {
		h.renderOrgActionsSettings(w, r, org, "variables", friendlyOrgActionsVariableError(err), "")
		return
	}
	h.recordOrgActionsAudit(r, viewer, audit.ActionActionsVariableSet, org.ID, name)
	http.Redirect(w, r, orgActionsSettingsPath(org.Slug, "variables")+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsVariableDelete(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	err := actionsvars.Deps{Pool: h.d.Pool}.Delete(r.Context(), actionsvars.OrgScope(org.ID), name)
	if err != nil {
		if errors.Is(err, actionsvars.ErrInvalidName) {
			http.Error(w, "bad variable name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordOrgActionsAudit(r, viewer, audit.ActionActionsVariableDeleted, org.ID, name)
	http.Redirect(w, r, orgActionsSettingsPath(org.Slug, "variables")+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) loadOrgSettingsOwner(w http.ResponseWriter, r *http.Request) (orgsdb.Org, bool) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return orgsdb.Org{}, false
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if !h.requireOrgOwner(w, r, org.ID, viewer) {
		return orgsdb.Org{}, false
	}
	return org, true
}

func (h *Handlers) renderOrgActionsSettings(w http.ResponseWriter, r *http.Request, org orgsdb.Org, kind, errMsg, notice string) {
	data := map[string]any{
		"CSRFToken":  middleware.CSRFTokenForRequest(r),
		"Org":        org,
		"Kind":       kind,
		"Error":      errMsg,
		"Notice":     notice,
		"FormAction": orgActionsSettingsPath(org.Slug, kind),
	}
	switch kind {
	case "secrets":
		items, err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.List(r.Context(), secrets.OrgScope(org.ID))
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "actions secrets: list org", "org_id", org.ID, "error", err)
			items = nil
		}
		data["Title"] = org.Slug + " · Actions secrets"
		data["Heading"] = "Actions secrets"
		data["IsSecrets"] = true
		data["Secrets"] = items
		data["SecretDisabled"] = h.d.SecretBox == nil
	case "variables":
		items, err := actionsvars.Deps{Pool: h.d.Pool}.List(r.Context(), actionsvars.OrgScope(org.ID))
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "actions variables: list org", "org_id", org.ID, "error", err)
			items = nil
		}
		data["Title"] = org.Slug + " · Actions variables"
		data["Heading"] = "Actions variables"
		data["Variables"] = items
	}
	h.d.Render.RenderPage(w, r, "orgs/settings_secrets", data)
}

func orgActionsSettingsPath(slug, kind string) string {
	return "/organizations/" + slug + "/settings/" + kind + "/actions"
}

func friendlyOrgActionsSecretError(err error) string {
	switch {
	case errors.Is(err, secrets.ErrInvalidName):
		return "Name must start with a letter or underscore and contain only letters, numbers, and underscores."
	case errors.Is(err, secrets.ErrEmptyValue):
		return "Secret value is required."
	case errors.Is(err, secrets.ErrInvalidScope):
		return "Invalid secret scope."
	default:
		return "Could not save secret."
	}
}

func friendlyOrgActionsVariableError(err error) string {
	switch {
	case errors.Is(err, actionsvars.ErrInvalidName):
		return "Name must start with a letter or underscore and contain only letters, numbers, and underscores."
	case errors.Is(err, actionsvars.ErrValueTooLong):
		return "Variable value must be 4096 characters or fewer."
	case errors.Is(err, actionsvars.ErrInvalidScope):
		return "Invalid variable scope."
	default:
		return "Could not save variable."
	}
}

func orgActionsNoticeMessage(code string) string {
	switch code {
	case "saved":
		return "Settings saved."
	case "deleted":
		return "Deleted."
	default:
		return ""
	}
}

func (h *Handlers) recordOrgActionsAudit(r *http.Request, viewer middleware.CurrentUser, action audit.Action, orgID int64, name string) {
	actor, meta := viewer.AuditActor(map[string]any{"name": name})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, action, audit.TargetOrg, orgID, meta)
}
