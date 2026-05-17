// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
)

func (h *Handlers) repoFeatureGate(ctx context.Context, row reposdb.Repo, feature entitlements.Feature) (entitlements.Decision, bool, error) {
	if !row.OwnerOrgID.Valid || row.Visibility != reposdb.RepoVisibilityPrivate {
		return entitlements.Decision{Feature: feature, Allowed: true}, false, nil
	}
	decision, err := entitlements.CheckOrgFeature(ctx, entitlements.Deps{Pool: h.d.Pool}, row.OwnerOrgID.Int64, feature)
	return decision, true, err
}

func (h *Handlers) repoFeatureBanner(ctx context.Context, row reposdb.Repo, ownerSlug string, feature entitlements.Feature, label string) (*entitlements.UpgradeBanner, bool) {
	decision, gated, err := h.repoFeatureGate(ctx, row, feature)
	if err != nil {
		h.d.Logger.WarnContext(ctx, "repo feature gate", "repo_id", row.ID, "feature", feature, "error", err)
		return nil, false
	}
	if !gated || decision.Allowed {
		return nil, true
	}
	banner := decision.UpgradeBanner(label, ownerSlug)
	return &banner, false
}

func (h *Handlers) requireRepoFeature(w http.ResponseWriter, r *http.Request, row reposdb.Repo, ownerSlug string, feature entitlements.Feature, label string) bool {
	decision, gated, err := h.repoFeatureGate(r.Context(), row, feature)
	if err != nil {
		h.d.Logger.WarnContext(r.Context(), "repo feature gate", "repo_id", row.ID, "feature", feature, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return false
	}
	if !gated || decision.Allowed {
		return true
	}
	banner := decision.UpgradeBanner(label, ownerSlug)
	h.d.Render.HTTPError(w, r, banner.StatusCode, banner.Message)
	return false
}
