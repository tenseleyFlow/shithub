// SPDX-License-Identifier: AGPL-3.0-or-later

package api

import (
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/orgs"
	orgsdb "github.com/tenseleyFlow/shithub/internal/orgs/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

func (h *Handlers) requireAPIOrgOwner(w http.ResponseWriter, r *http.Request, org orgsdb.Org) bool {
	auth := middleware.PATAuthFromContext(r.Context())
	if auth.UserID == 0 {
		writeAPIError(w, http.StatusUnauthorized, "unauthenticated")
		return false
	}
	if auth.IsSuspended {
		writeAPIError(w, http.StatusForbidden, "account is suspended")
		return false
	}
	if auth.IsSiteAdmin {
		return true
	}

	odeps := orgs.Deps{Pool: h.d.Pool, Logger: h.d.Logger}
	isMember, err := orgs.IsMember(r.Context(), odeps, org.ID, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org member check", "org_id", org.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "authorization failed")
		return false
	}
	if !isMember {
		writeAPIError(w, http.StatusNotFound, "org not found")
		return false
	}

	isOwner, err := orgs.IsOwner(r.Context(), odeps, org.ID, auth.UserID)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org owner check", "org_id", org.ID, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "authorization failed")
		return false
	}
	if !isOwner {
		writeAPIError(w, http.StatusForbidden, "organization owner access required")
		return false
	}
	return true
}

func (h *Handlers) requireOrgFeature(w http.ResponseWriter, r *http.Request, org orgsdb.Org, feature entitlements.Feature, label string) bool {
	decision, err := entitlements.CheckOrgFeature(r.Context(), entitlements.Deps{Pool: h.d.Pool}, org.ID, feature)
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "api: org entitlement check", "org_id", org.ID, "feature", feature, "error", err)
		writeAPIError(w, http.StatusInternalServerError, "entitlement check failed")
		return false
	}
	if !decision.Allowed {
		banner := decision.UpgradeBanner(label, org.Slug)
		writeAPIError(w, banner.StatusCode, banner.Message)
		return false
	}
	return true
}
