// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	actionsvars "github.com/tenseleyFlow/shithub/internal/actions/variables"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/policy"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// MountSettingsActions registers the Actions secrets + variables settings
// routes. Caller wraps with RequireUser; per-route policy gates inside.
func (h *Handlers) MountSettingsActions(r chi.Router) {
	r.Get("/{owner}/{repo}/settings/secrets/actions", h.settingsActionsSecrets)
	r.Post("/{owner}/{repo}/settings/secrets/actions", h.settingsActionsSecretSet)
	r.Post("/{owner}/{repo}/settings/secrets/actions/{name}/delete", h.settingsActionsSecretDelete)
	r.Get("/{owner}/{repo}/settings/variables/actions", h.settingsActionsVariables)
	r.Post("/{owner}/{repo}/settings/variables/actions", h.settingsActionsVariableSet)
	r.Post("/{owner}/{repo}/settings/variables/actions/{name}/delete", h.settingsActionsVariableDelete)
}

func (h *Handlers) settingsActionsSecrets(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsSettings(w, r, row, owner.Username, "secrets", "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsSecretSet(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
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
	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Set(r.Context(), secrets.RepoScope(row.ID), name, value, viewer.ID)
	if err != nil {
		h.renderRepoActionsSettings(w, r, row, owner.Username, "secrets", friendlyActionsSecretError(err), "")
		return
	}
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsSecretSet, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "secrets")+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsSecretDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.Delete(r.Context(), secrets.RepoScope(row.ID), name)
	if err != nil {
		if errors.Is(err, secrets.ErrInvalidName) {
			http.Error(w, "bad secret name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsSecretDeleted, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "secrets")+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsVariables(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	h.renderRepoActionsSettings(w, r, row, owner.Username, "variables", "", settingsNoticeMessage(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsActionsVariableSet(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
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
	err := actionsvars.Deps{Pool: h.d.Pool}.Set(r.Context(), actionsvars.RepoScope(row.ID), name, value, viewer.ID)
	if err != nil {
		h.renderRepoActionsSettings(w, r, row, owner.Username, "variables", friendlyActionsVariableError(err), "")
		return
	}
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsVariableSet, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "variables")+"?notice=saved", http.StatusSeeOther)
}

func (h *Handlers) settingsActionsVariableDelete(w http.ResponseWriter, r *http.Request) {
	row, owner, ok := h.loadRepoAndAuthorize(w, r, policy.ActionRepoSettingsActions)
	if !ok {
		return
	}
	name := chi.URLParam(r, "name")
	err := actionsvars.Deps{Pool: h.d.Pool}.Delete(r.Context(), actionsvars.RepoScope(row.ID), name)
	if err != nil {
		if errors.Is(err, actionsvars.ErrInvalidName) {
			http.Error(w, "bad variable name", http.StatusBadRequest)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	h.recordRepoActionsAudit(r, viewer, audit.ActionActionsVariableDeleted, row.ID, name)
	http.Redirect(w, r, repoActionsSettingsPath(owner.Username, row.Name, "variables")+"?notice=deleted", http.StatusSeeOther)
}

func (h *Handlers) renderRepoActionsSettings(w http.ResponseWriter, r *http.Request, row reposdb.Repo, owner, kind, errMsg, notice string) {
	data := map[string]any{
		"CSRFToken": middleware.CSRFTokenForRequest(r),
		"Owner":     owner,
		"Repo":      row,
		"Kind":      kind,
		"Error":     errMsg,
		"Notice":    notice,
	}
	switch kind {
	case "secrets":
		items, err := secrets.Deps{Pool: h.d.Pool, Box: h.d.SecretBox}.List(r.Context(), secrets.RepoScope(row.ID))
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "actions secrets: list repo", "repo_id", row.ID, "error", err)
			items = nil
		}
		data["Title"] = "Actions secrets · " + row.Name
		data["Heading"] = "Actions secrets"
		data["IsSecrets"] = true
		data["Secrets"] = items
		data["SecretDisabled"] = h.d.SecretBox == nil
		data["SettingsActive"] = "actions-secrets"
		data["FormAction"] = repoActionsSettingsPath(owner, row.Name, "secrets")
	case "variables":
		items, err := actionsvars.Deps{Pool: h.d.Pool}.List(r.Context(), actionsvars.RepoScope(row.ID))
		if err != nil {
			h.d.Logger.WarnContext(r.Context(), "actions variables: list repo", "repo_id", row.ID, "error", err)
			items = nil
		}
		data["Title"] = "Actions variables · " + row.Name
		data["Heading"] = "Actions variables"
		data["Variables"] = items
		data["SettingsActive"] = "actions-variables"
		data["FormAction"] = repoActionsSettingsPath(owner, row.Name, "variables")
	}
	h.d.Render.RenderPage(w, r, "repo/settings_secrets", data)
}

func repoActionsSettingsPath(owner, repoName, kind string) string {
	return "/" + owner + "/" + repoName + "/settings/" + kind + "/actions"
}

func friendlyActionsSecretError(err error) string {
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

func friendlyActionsVariableError(err error) string {
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

func (h *Handlers) recordRepoActionsAudit(r *http.Request, viewer middleware.CurrentUser, action audit.Action, repoID int64, name string) {
	actor, meta := viewer.AuditActor(map[string]any{"name": name})
	_ = h.d.Audit.Record(r.Context(), h.d.Pool, actor, action, audit.TargetRepo, repoID, meta)
}
