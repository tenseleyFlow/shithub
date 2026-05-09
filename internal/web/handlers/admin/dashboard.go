// SPDX-License-Identifier: AGPL-3.0-or-later

package admin

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// dashboard surfaces the operator at-a-glance counts: active users,
// suspended, repos, orgs, jobs by status. Cheap aggregates only —
// anything expensive should live behind its own page.
func (h *Handlers) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pool := h.d.Pool

	users, _ := h.aq.CountActiveUsers(ctx, pool)
	suspended, _ := h.aq.CountSuspendedUsers(ctx, pool)
	admins, _ := h.aq.CountSiteAdmins(ctx, pool)
	repos, _ := h.aq.CountActiveRepos(ctx, pool)
	orgs, _ := h.aq.CountActiveOrgs(ctx, pool)
	jobs, _ := h.aq.CountJobsByStatus(ctx, pool)

	h.d.Render.RenderPage(w, r, "admin/dashboard", map[string]any{
		"Title":       "Site admin",
		"CSRFToken":   middleware.CSRFTokenForRequest(r),
		"AdminActive": "dashboard",
		"SiteName":    h.d.SiteName,
		"Counts": map[string]int64{
			"users":     users,
			"suspended": suspended,
			"admins":    admins,
			"repos":     repos,
			"orgs":      orgs,
		},
		"JobsByStatus": jobs,
	})
}
