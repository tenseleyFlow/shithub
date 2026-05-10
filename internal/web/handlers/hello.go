// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/version"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

type helloHandler struct {
	render  *render.Renderer
	logoSVG string
	logger  *slog.Logger
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
}

func (h helloHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data := helloData{
		Title:     "Welcome",
		Version:   version.Version,
		Commit:    version.Commit,
		BuiltAt:   version.BuiltAt,
		LogoSVG:   template.HTML(h.logoSVG), // #nosec G203 — embedded server-owned asset
		Viewer:    middleware.CurrentUserFromContext(r.Context()),
		CSRFToken: middleware.CSRFTokenForRequest(r),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.render.RenderPage(w, r, "hello", data); err != nil {
		h.logger.Error("render hello", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
