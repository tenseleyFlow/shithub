// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/social"
	"github.com/tenseleyFlow/shithub/internal/version"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

type helloHandler struct {
	render  *render.Renderer
	logoSVG string
	logger  *slog.Logger
	pool    *pgxpool.Pool
}

type helloData struct {
	Title   string
	Version string
	Commit  string
	BuiltAt string
	LogoSVG template.HTML
	// Viewer + CSRFToken mirror the fields _nav.html branches on. Typed
	// page-data structs must populate them explicitly — the renderer
	// only auto-injects for map[string]any data.
	Viewer    middleware.CurrentUser
	CSRFToken string
	// OG* are referenced by the shared _layout.html (S09). The fields
	// must exist on every typed page-data struct that goes through the
	// layout — html/template evaluates `{{ if .X }}` even on nil-checks
	// and errors when X is missing.
	OGTitle       string
	OGDescription string
	OGImage       string
	// GlobalSearchQuery is referenced by _nav.html's search input to
	// preserve the query when re-rendering after a search. Hello has
	// no query of its own, but the field must exist or template
	// execution errors out.
	GlobalSearchQuery string
	// Repo/Org are optional nav contexts. They are nil on the home page,
	// but typed data must still expose them because _nav.html probes
	// these fields before deciding whether to render context tabs.
	Repo any
	Org  any
}

func (h helloHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	viewer := middleware.CurrentUserFromContext(r.Context())
	if viewer.ID != 0 && h.pool != nil {
		h.serveDashboard(w, r, viewer)
		return
	}

	data := helloData{
		Title:     "Welcome",
		Version:   version.Version,
		Commit:    version.Commit,
		BuiltAt:   version.BuiltAt,
		LogoSVG:   template.HTML(h.logoSVG), // #nosec G203 — embedded server-owned asset
		Viewer:    viewer,
		CSRFToken: middleware.CSRFTokenForRequest(r),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.render.RenderPage(w, r, "hello", data); err != nil {
		h.logger.Error("render hello", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}

func (h helloHandler) serveDashboard(w http.ResponseWriter, r *http.Request, viewer middleware.CurrentUser) {
	deps := social.Deps{Pool: h.pool, Logger: h.logger}
	feed, hasNext, nextURL := feedPageFor(r, func(cursor social.FeedCursor, limit int32) ([]social.FeedItem, error) {
		return social.DashboardFeed(r.Context(), deps, viewer.ID, cursor, limit)
	})
	repos, err := social.DashboardRepos(r.Context(), deps, viewer.ID, 8)
	if err != nil && h.logger != nil {
		h.logger.WarnContext(r.Context(), "dashboard repos", "error", err)
	}
	trendingRepos, err := social.CachedTrendingRepos(r.Context(), deps, social.TrendingScopeWeek, 7, 5)
	if err != nil && h.logger != nil {
		h.logger.WarnContext(r.Context(), "dashboard trending repos", "error", err)
	}
	trendingUsers, err := social.CachedTrendingUsers(r.Context(), deps, social.TrendingScopeWeek, 7, 5)
	if err != nil && h.logger != nil {
		h.logger.WarnContext(r.Context(), "dashboard trending users", "error", err)
	}

	data := map[string]any{
		"Title":         "Home",
		"TopRepos":      repos,
		"Feed":          feed,
		"FeedHasNext":   hasNext,
		"FeedNextURL":   nextURL,
		"TrendingRepos": trendingRepos,
		"TrendingUsers": trendingUsers,
	}
	if err := h.render.RenderPage(w, r, "dashboard", data); err != nil {
		if h.logger != nil {
			h.logger.Error("render dashboard", "error", err)
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
