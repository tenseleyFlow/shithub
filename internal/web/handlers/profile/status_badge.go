// SPDX-License-Identifier: AGPL-3.0-or-later

package profile

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	authpkg "github.com/tenseleyFlow/shithub/internal/auth"
	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/statuspage"
)

// serveStatusBadge handles GET /{slug}.status.svg, returning the SVG
// badge for a user's personal status (PRO-EXT01-14).
//
// Free users get the 402-style "Pro feature" SVG so the badge endpoint
// is discoverable but doesn't leak aggregate data. The endpoint always
// returns 200 with a valid SVG — markdown renderers and badge
// aggregators don't retry well on 4xx, and a missing image breaks the
// visual flow of any README that already embeds the URL.
func (h *Handlers) serveStatusBadge(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rawName := chi.URLParam(r, "slug")
	lower := strings.ToLower(rawName)
	if authpkg.IsReserved(lower) {
		writeBadgeNotFound(w)
		return
	}

	user, err := h.q.GetUserByUsername(ctx, h.d.Pool, rawName)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeBadgeNotFound(w)
			return
		}
		h.d.Logger.ErrorContext(ctx, "status badge: user lookup", "error", err)
		writeBadgeNotFound(w)
		return
	}
	if user.SuspendedAt.Valid || user.DeletedAt.Valid {
		writeBadgeNotFound(w)
		return
	}

	ownerIsPro := billing.IsProUserPlan(billing.UserPlan(user.Plan))
	allowed := h.statusPageGateAllowed(ctx, user.ID, ownerIsPro)

	if !allowed {
		// 402 Payment Required on the body but 200 on the status —
		// see the docstring above for the rationale. The SVG itself
		// reads "Pro feature" so the visual signal is unambiguous.
		writeSVG(w, statuspage.BuildPaidBadge(), "no-store")
		return
	}

	summary, err := statuspage.Aggregate(ctx, h.d.Pool, user.ID, user.Username)
	if err != nil {
		h.d.Logger.ErrorContext(ctx, "status badge: aggregate", "user_id", user.ID, "error", err)
		writeSVG(w, statuspage.BuildBadge(statuspage.StateUnknown), "max-age=60")
		return
	}
	// Same 60s cache as the page — keep the two views consistent so
	// a README image doesn't outrun the page that backs it.
	writeSVG(w, statuspage.BuildBadge(summary.OverallState), "max-age=60")
}

// writeSVG sets the SVG content-type + a no-sniff guard + the caller-
// supplied cache directive. nosniff defends against badge-aggregator
// pipelines that might otherwise sniff the body as HTML.
func writeSVG(w http.ResponseWriter, body []byte, cacheControl string) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if cacheControl != "" {
		w.Header().Set("Cache-Control", cacheControl)
	}
	w.WriteHeader(http.StatusOK)
	//nolint:gosec // G705: body is server-generated SVG from statuspage.Build*.
	_, _ = w.Write(body)
}

// writeBadgeNotFound returns an "unknown" badge for users that don't
// exist / are suspended. Reuses the unknown state's grey palette so
// the badge degrades visually without leaking existence info.
func writeBadgeNotFound(w http.ResponseWriter) {
	writeSVG(w, statuspage.BuildBadge(statuspage.StateUnknown), "no-store")
}
