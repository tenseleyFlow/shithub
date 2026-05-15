// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/entitlements"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
)

// PRO-EXT01-11c: per-token usage analytics page.
//
// Free users see the page but with a "preview" payload: synthetic
// numbers plus a Pro upgrade CTA. The data itself is collected for
// every token regardless of plan (capture in the PAT middleware) —
// gating happens at the *display* layer so the user has a continuous
// upgrade story without losing historical data.

const (
	// analyticsWindow is how far back the analytics view summarizes.
	analyticsWindow = 30 * 24 * time.Hour
	// analyticsTopRoutesCap caps the "top routes" table.
	analyticsTopRoutesCap = 10
)

// tokenAnalytics renders /settings/tokens/{id}/analytics.
func (h *Handlers) tokenAnalytics(w http.ResponseWriter, r *http.Request) {
	user := middleware.CurrentUserFromContext(r.Context())
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "token not found")
		return
	}

	tok, err := h.q.GetUserTokenByIDForUser(r.Context(), h.d.Pool,
		usersdb.GetUserTokenByIDForUserParams{ID: id, UserID: user.ID})
	if err != nil {
		h.d.Render.HTTPError(w, r, http.StatusNotFound, "token not found")
		return
	}

	allowed, _, _ := h.userFineGrainedPATsAllowed(r.Context(), user.ID)

	data := map[string]any{
		"Title":          "Token analytics",
		"CSRFToken":      middleware.CSRFTokenForRequest(r),
		"SettingsActive": "tokens",
		"Token":          tok,
		"Allowed":        allowed,
		"FineGrainedKey": string(entitlements.FeatureFineGrainedPATs),
	}

	if !allowed {
		// Preview payload: synthetic numbers with the Pro CTA. Keeps
		// the page useful as a teaser without leaking actual data.
		data["TotalRequests"] = int64(0)
		data["DailyCounts"] = preview7DayCounts()
		data["TopRoutes"] = preview3TopRoutes()
		data["IsPreview"] = true
		banner := entitlements.Decision{}.PrincipalUpgradeBanner(
			"PAT usage analytics", billing.PrincipalForUser(user.ID), "",
		)
		data["UpgradeBanner"] = banner.Message
		h.renderPage(w, r, "settings/token_analytics", data)
		return
	}

	since := time.Now().Add(-analyticsWindow)
	total, err := h.q.CountUserTokenUsageSince(r.Context(), h.d.Pool, usersdb.CountUserTokenUsageSinceParams{
		TokenID:    tok.ID,
		OccurredAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "token analytics: count", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	dailyRows, err := h.q.ListUserTokenUsageByDay(r.Context(), h.d.Pool, usersdb.ListUserTokenUsageByDayParams{
		TokenID:    tok.ID,
		OccurredAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "token analytics: daily", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}
	topRoutes, err := h.q.ListUserTokenTopRoutes(r.Context(), h.d.Pool, usersdb.ListUserTokenTopRoutesParams{
		TokenID:    tok.ID,
		OccurredAt: pgtype.Timestamptz{Time: since, Valid: true},
		Limit:      int32(analyticsTopRoutesCap),
	})
	if err != nil {
		h.d.Logger.ErrorContext(r.Context(), "token analytics: top routes", "error", err)
		h.d.Render.HTTPError(w, r, http.StatusInternalServerError, "")
		return
	}

	data["TotalRequests"] = total
	data["DailyCounts"] = dailyRowsToView(dailyRows)
	data["TopRoutes"] = topRoutes
	data["IsPreview"] = false
	h.renderPage(w, r, "settings/token_analytics", data)
}

// dayCount is the template view of one bucket in the daily series.
type dayCount struct {
	Day   string
	Count int64
}

func dailyRowsToView(rows []usersdb.ListUserTokenUsageByDayRow) []dayCount {
	out := make([]dayCount, len(rows))
	for i, row := range rows {
		out[i] = dayCount{
			Day:   row.Day.Time.Format("2006-01-02"),
			Count: row.EventCount,
		}
	}
	return out
}

// preview7DayCounts returns a synthetic 7-day series for the Free
// preview. The shape matters more than the numbers — readers should
// see "oh, my future analytics will look like a chart" but never
// mistake the preview for real data.
func preview7DayCounts() []dayCount {
	day := time.Now().AddDate(0, 0, -6)
	counts := []int64{12, 17, 9, 22, 30, 8, 14}
	out := make([]dayCount, 7)
	for i, c := range counts {
		out[i] = dayCount{
			Day:   day.AddDate(0, 0, i).Format("2006-01-02"),
			Count: c,
		}
	}
	return out
}

// preview3TopRoutes returns a synthetic top-3 set for the Free preview.
func preview3TopRoutes() []usersdb.ListUserTokenTopRoutesRow {
	return []usersdb.ListUserTokenTopRoutesRow{
		{Method: "GET", RoutePrefix: "/api/v1/repos", EventCount: 42},
		{Method: "POST", RoutePrefix: "/api/v1/repos", EventCount: 11},
		{Method: "GET", RoutePrefix: "/api/v1/user", EventCount: 7},
	}
}
