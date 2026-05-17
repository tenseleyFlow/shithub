// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	"github.com/tenseleyFlow/shithub/internal/statuspage"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// serveStatus renders the personal status page at /{username}/status
// (PRO-EXT01-14). Pro users see the live aggregate; Free users get a
// teaser with placeholder data so the surface is discoverable.
//
// The aggregator only ever runs for Pro owners — Free pages render the
// static teaser without touching workflow_runs at all, so the page is
// cheap even under scrape load.
func (h *Handlers) serveStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawName := chi.URLParam(r, "username")
	lower := strings.ToLower(rawName)
	if authpkg.IsReserved(lower) {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
		return
	}

	user, err := h.q.GetUserByUsername(ctx, h.d.Pool, rawName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			h.tryRedirectOrNotFound(w, r, lower)
			return
		}
		h.d.Logger.ErrorContext(ctx, "status page: user lookup", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	if rawName != user.Username {
		http.Redirect(w, r, "/"+user.Username+"/status", http.StatusMovedPermanently)
		return
	}
	if user.SuspendedAt.Valid || user.DeletedAt.Valid {
		h.renderUnavailable(w, r, user.Username)
		return
	}

	viewer := middleware.CurrentUserFromContext(ctx)
	ownerIsPro := billing.IsProUserPlan(billing.UserPlan(user.Plan))
	allowed := h.statusPageGateAllowed(ctx, user.ID, ownerIsPro)

	displayName := user.DisplayName
	if displayName == "" {
		displayName = user.Username
	}
	avatarURL := fmt.Sprintf("/avatars/%s", url.PathEscape(user.Username))
	badgeURL := "/" + url.PathEscape(user.Username) + ".status.svg"

	data := map[string]any{
		"Title":             displayName + " — status",
		"User":              user,
		"DisplayName":       displayName,
		"AvatarURL":         avatarURL,
		"BadgeURL":          badgeURL,
		"IsSelf":            viewer.ID != 0 && viewer.ID == user.ID,
		"ProfileOwnerIsPro": ownerIsPro,
	}

	if !allowed {
		// Free + enforce on: serve the teaser. Placeholder rows show
		// the shape of the page so a Free user can decide whether
		// the live data would be valuable — same idea as the
		// scheduled-issues "preview chart" teaser.
		w.Header().Set("Cache-Control", "no-cache, private")
		summary := teaserSummary()
		data["Summary"] = summary
		data["IsTeaser"] = true
		data["TeaserUpgradeHref"] = "/settings/billing"
		if err := h.d.Render.RenderPage(w, r, "profile/status", data); err != nil {
			h.d.Logger.ErrorContext(ctx, "status page: render teaser", "error", err)
		}
		return
	}

	// Pro path (or Free in report-only soak): real aggregate. Even for
	// a Pro user with zero pinned repos the aggregator returns a
	// renderable Summary{OverallState: unknown}.
	summary, err := statuspage.Aggregate(ctx, h.d.Pool, user.ID, user.Username)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "status page: aggregate", "user_id", user.ID, "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	// Short cache so the page stays fresh after a run completes but
	// rapid refreshes don't re-run the aggregator. Pagecache
	// invalidation on run completion is out of scope for this sprint;
	// 60s is the floor of "feels live without thrashing the DB."
	w.Header().Set("Cache-Control", "max-age=60, private")
	data["Summary"] = summary
	data["IsTeaser"] = false
	if err := h.d.Render.RenderPage(w, r, "profile/status", data); err != nil {
		h.d.Logger.ErrorContext(ctx, "status page: render", "error", err)
	}
}

// statusPageGateAllowed returns true when the live aggregate should be
// rendered. Free users with enforce on get false (→ teaser); report-
// only soak still serves the live page so we can sanity-check the
// aggregator against real data before flipping the flag.
//
// The deny-side decision is logged with surface=status-page so SREs
// can spot a misconfigured plan before the user complains.
func (h *Handlers) statusPageGateAllowed(ctx context.Context, userID int64, ownerIsPro bool) bool {
	decision, err := entitlements.CheckPrincipalFeature(ctx,
		entitlements.Deps{Pool: h.d.Pool},
		billing.PrincipalForUser(userID),
		entitlements.FeaturePersonalStatusPage)
	if err != nil {
		// Fail open in report-only, fail closed under enforce — same
		// shape every other gate uses so an entitlement-DB blip
		// doesn't accidentally unlock features.
		if h.d.BillingEnforce.UserPersonalStatusPage {
			return false
		}
		return ownerIsPro
	}
	if !decision.Allowed && h.d.Logger != nil {
		mode := "report_only"
		if h.d.BillingEnforce.UserPersonalStatusPage {
			mode = "enforce"
		}
		h.d.Logger.InfoContext(ctx, "entitlements.report_only_deny",
			"principal", billing.PrincipalForUser(userID).String(),
			"principal_kind", string(billing.SubjectKindUser),
			"principal_id", userID,
			"feature", string(entitlements.FeaturePersonalStatusPage),
			"reason", string(decision.Reason),
			"required_plan", string(decision.RequiredPlan),
			"mode", mode,
			"surface", "status-page")
	}
	if !decision.Allowed && h.d.BillingEnforce.UserPersonalStatusPage {
		return false
	}
	return true
}

// teaserSummary builds a static placeholder Summary used for Free-user
// renders. Three rows give a sense of how the live page would look
// without any per-user data — names are obviously fake to avoid being
// mistaken for real status.
func teaserSummary() statuspage.Summary {
	now := time.Now().UTC()
	return statuspage.Summary{
		OverallState: statuspage.StateOK,
		GeneratedAt:  now,
		Repos: []statuspage.RepoStatus{
			{
				Owner:         "you",
				Name:          "your-first-repo",
				DefaultBranch: "main",
				LatestRun: statuspage.LatestRun{
					Conclusion:  "success",
					CompletedAt: now.Add(-15 * time.Minute),
					RunIndex:    42,
				},
				SuccessRate: 0.97,
				TotalRuns:   58,
			},
			{
				Owner:         "you",
				Name:          "another-project",
				DefaultBranch: "main",
				LatestRun: statuspage.LatestRun{
					Conclusion:  "failure",
					CompletedAt: now.Add(-2 * time.Hour),
					RunIndex:    7,
				},
				SuccessRate: 0.82,
				TotalRuns:   17,
			},
			{
				Owner:         "you",
				Name:          "side-quest",
				DefaultBranch: "trunk",
				LatestRun: statuspage.LatestRun{
					Conclusion:  "success",
					CompletedAt: now.Add(-26 * time.Hour),
					RunIndex:    113,
				},
				SuccessRate: 1.0,
				TotalRuns:   9,
			},
		},
	}
}
