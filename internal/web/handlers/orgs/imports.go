// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	orgdomain "github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type importForm struct {
	GitHubOrg   string
	GitHubToken string
}

func (h *Handlers) settingsImport(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	h.renderSettingsImport(w, r, org, importForm{}, "", importNotice(r.URL.Query().Get("notice")))
}

func (h *Handlers) settingsImportSubmit(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.d.Render.HTTPError(w, r, http.StatusBadRequest, "form parse")
		return
	}
	form := importForm{
		GitHubOrg:   strings.TrimSpace(r.PostFormValue("github_org")),
		GitHubToken: strings.TrimSpace(r.PostFormValue("github_token")),
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	imp, err := orgdomain.StartGitHubImport(r.Context(), orgdomain.ImportDeps{
		Pool: h.d.Pool, Box: h.d.SecretBox, Logger: h.d.Logger,
	}, orgdomain.StartGitHubImportParams{
		OrgID: org.ID, SourceOrg: form.GitHubOrg,
		RequestedByUserID: viewer.ID, Token: form.GitHubToken,
	})
	if err != nil {
		h.renderSettingsImport(w, r, org, form.withoutToken(), friendlyImportError(err), "")
		return
	}
	http.Redirect(w, r, "/organizations/"+org.Slug+"/imports/"+strconv.FormatInt(imp.ID, 10), http.StatusSeeOther)
}

func (f importForm) withoutToken() importForm {
	f.GitHubToken = ""
	return f
}

func (h *Handlers) importProgress(w http.ResponseWriter, r *http.Request) {
	org, ok := h.loadOrgSettingsOwner(w, r)
	if !ok {
		return
	}
	importID, err := strconv.ParseInt(chi.URLParam(r, "importID"), 10, 64)
	if err != nil || importID <= 0 {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}
	q := orgsdb.New()
	progress, err := q.GetOrgGithubImportProgress(r.Context(), h.d.Pool, orgsdb.GetOrgGithubImportProgressParams{
		ID:    importID,
		OrgID: org.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
			return
		}
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	items, err := q.ListOrgGithubImportRepos(r.Context(), h.d.Pool, importID)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if err := h.d.Render.RenderPage(w, r, "orgs/import_progress", map[string]any{
		"Title":        org.Slug + " - GitHub import",
		"CSRFToken":    middleware.CSRFTokenForRequest(r),
		"Org":          org,
		"AvatarURL":    "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav": "settings",
		"Progress":     progress,
		"Items":        items,
		"IsTerminal":   orgdomain.IsTerminalImportStatus(progress.Status),
	}); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org import: render progress", "error", err)
	}
}

func (h *Handlers) renderSettingsImport(w http.ResponseWriter, r *http.Request, org orgsdb.Org, form importForm, errMsg, notice string) {
	imports, err := orgsdb.New().ListOrgGithubImportsForOrg(r.Context(), h.d.Pool, orgsdb.ListOrgGithubImportsForOrgParams{
		OrgID: org.ID,
		Limit: 10,
	})
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "org import: list imports", "error", err, "org_id", org.ID)
	}
	_ = h.d.Render.RenderPage(w, r, "orgs/settings_import", map[string]any{
		"Title":             org.Slug + " - repository import",
		"CSRFToken":         middleware.CSRFTokenForRequest(r),
		"Org":               org,
		"AvatarURL":         "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav":      "settings",
		"OrgSettingsActive": "import",
		"BillingEnabled":    h.d.BillingEnabled,
		"Form":              form,
		"Error":             errMsg,
		"Notice":            notice,
		"Imports":           imports,
		"SecretBoxOK":       h.d.SecretBox != nil,
	})
}

func friendlyImportError(err error) string {
	switch {
	case errors.Is(err, orgdomain.ErrInvalidGitHubOrg):
		return "GitHub organization must be a valid organization name or github.com organization URL."
	case errors.Is(err, orgdomain.ErrImportTokenKeyNeeded):
		return "GitHub token imports require the server secret key to be configured."
	default:
		return "Could not start the GitHub organization import."
	}
}

func importNotice(code string) string {
	switch code {
	case "start-failed":
		return "The organization was created, but the import could not be started. Try again here."
	default:
		return ""
	}
}
