// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"net/http"
	"net/url"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	secretscandb "github.com/tenseleyFlow/shithub/internal/secretscan/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) securityOverview(w http.ResponseWriter, r *http.Request) {
	org, ok := h.orgFromSlug(w, r)
	if !ok {
		return
	}
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.IsAnonymous() {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}

	isOwner := false
	isMember := false
	if viewer.IsSiteAdmin {
		isMember = true
	} else {
		deps := h.deps()
		isOwner, _ = orgs.IsOwner(r.Context(), deps, org.ID, viewer.ID)
		isMember, _ = orgs.IsMember(r.Context(), deps, org.ID, viewer.ID)
	}
	if !isMember {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "")
		return
	}

	decision, err := entitlements.CheckOrgFeature(r.Context(), entitlements.Deps{Pool: h.d.Pool}, org.ID, entitlements.FeatureSecurityOverview)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: entitlement check", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	navCounts := h.orgNavCounts(r.Context(), org.ID, -1)
	data := map[string]any{
		"Title":        org.Slug + " · Security and quality",
		"Org":          org,
		"AvatarURL":    "/avatars/" + url.PathEscape(org.Slug),
		"ActiveOrgNav": "security",
		"RepoCount":    navCounts.RepoCount,
		"MemberCount":  navCounts.MemberCount,
		"TeamCount":    navCounts.TeamCount,
		"IsOwner":      isOwner,
		"IsMember":     true,
		"Locked":       !decision.Allowed,
	}
	if !decision.Allowed {
		w.WriteHeader(decision.HTTPStatus())
		data["UpgradeBanner"] = decision.UpgradeBanner("Security overview features", string(org.Slug))
		if err := h.d.Render.RenderPage(w, r, "orgs/security", data); err != nil {
			h.d.Logger.ErrorContext(r.Context(), "org security: render locked", "org_id", org.ID, "error", err)
		}
		return
	}

	q := reposdb.New()
	sq := secretscandb.New()
	orgID := pgtype.Int8{Int64: org.ID, Valid: true}
	summary, err := q.OrgSecurityOverviewSummary(r.Context(), h.d.Pool, orgID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: summary", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	alerts, err := q.ListOrgDependencyAlerts(r.Context(), h.d.Pool, reposdb.ListOrgDependencyAlertsParams{
		OwnerOrgID: orgID,
		Limit:      25,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: dependency alerts", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	repos, err := q.ListOrgSecurityRepoSummaries(r.Context(), h.d.Pool, reposdb.ListOrgSecurityRepoSummariesParams{
		OwnerOrgID: orgID,
		Limit:      25,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: repository summaries", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	advisories, err := q.ListOrgSecurityAdvisories(r.Context(), h.d.Pool, reposdb.ListOrgSecurityAdvisoriesParams{
		OwnerOrgID: orgID,
		Limit:      25,
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: repository advisories", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	secretDecision, err := entitlements.CheckOrgFeature(r.Context(), entitlements.Deps{Pool: h.d.Pool}, org.ID, entitlements.FeatureSecretScanning)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: secret scanning entitlement check", "org_id", org.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	data["SecretScanningAllowed"] = secretDecision.Allowed
	if secretDecision.Allowed {
		secretSummary, err := sq.OrgSecretScanSummary(r.Context(), h.d.Pool, orgID)
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "org security: secret scan summary", "org_id", org.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		secretFindings, err := sq.ListOrgSecretScanFindings(r.Context(), h.d.Pool, secretscandb.ListOrgSecretScanFindingsParams{
			OwnerOrgID: orgID,
			Limit:      25,
		})
		if err != nil {
			h.d.Logger.ErrorContext(r.Context(), "org security: secret scan findings", "org_id", org.ID, "error", err)
			h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
			return
		}
		data["SecretSummary"] = secretSummary
		data["SecretFindings"] = secretFindings
	} else {
		data["SecretUpgradeBanner"] = secretDecision.UpgradeBanner("Secret scanning", string(org.Slug))
	}

	data["Summary"] = summary
	data["Alerts"] = alerts
	data["Repositories"] = repos
	data["Advisories"] = advisories
	if err := h.d.Render.RenderPage(w, r, "orgs/security", data); err != nil {
		h.d.Logger.ErrorContext(r.Context(), "org security: render", "org_id", org.ID, "error", err)
	}
}
