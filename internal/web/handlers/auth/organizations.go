// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"net/url"

	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

type settingsOrganization struct {
	Slug        string
	DisplayName string
	RoleLabel   string
	AvatarURL   string
	CanManage   bool
}

// settingsOrganizations renders GET /settings/organizations.
func (h *Handlers) settingsOrganizations(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	rows, err := orgsdb.New().ListOrgsForUser(r.Context(), h.d.Pool, user.ID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "settings/organizations: list", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	organizations := make([]settingsOrganization, 0, len(rows))
	for _, row := range rows {
		displayName := row.DisplayName
		if displayName == "" {
			displayName = row.Slug
		}
		organizations = append(organizations, settingsOrganization{
			Slug:        row.Slug,
			DisplayName: displayName,
			RoleLabel:   settingsOrgRoleLabel(row.Role),
			AvatarURL:   "/avatars/" + url.PathEscape(row.Slug),
			CanManage:   row.Role == orgsdb.OrgRoleOwner,
		})
	}

	h.renderPage(w, r, "settings/organizations", map[string]any{
		"Title":          "Organizations",
		"SettingsActive": "organizations",
		"Username":       user.Username,
		"Organizations":  organizations,
	})
}

func settingsOrgRoleLabel(role orgsdb.OrgRole) string {
	switch role {
	case orgsdb.OrgRoleOwner:
		return "Owner"
	case orgsdb.OrgRoleMember:
		return "Member"
	default:
		return string(role)
	}
}
