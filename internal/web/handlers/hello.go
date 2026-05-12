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
	baseURL string
	logger  *slog.Logger
}

type helloData struct {
	Title   string
	Version string
	Commit  string
	BuiltAt string
	LogoSVG template.HTML
	// Viewer + CSRFToken mirror the fields _nav.html branches on. Typed
	// page-data structs must populate them explicitly - the renderer
	// only auto-injects for map[string]any data.
	Viewer    middleware.CurrentUser
	CSRFToken string
	// SEO/social fields are read by optional layout helpers, so typed
	// page-data structs may provide only the fields they actually need.
	MetaDescription string
	CanonicalURL    string
	OGTitle         string
	OGDescription   string
	OGImage         string
	OGType          string
	StructuredData  template.JS
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
	data := helloData{
		Title:           "Welcome",
		Version:         version.Version,
		Commit:          version.Commit,
		BuiltAt:         version.BuiltAt,
		LogoSVG:         template.HTML(h.logoSVG), // #nosec G203 - embedded server-owned asset
		Viewer:          viewer,
		CSRFToken:       middleware.CSRFTokenForRequest(r),
		MetaDescription: defaultMetaDescription,
		CanonicalURL:    canonicalURL(h.baseURL, r, "/"),
		OGTitle:         "shithub: GitHub-style git hosting without Copilot",
		OGDescription:   "A self-hostable, AGPL GitHub alternative with familiar repositories, pull requests, issues, organizations, code search, and Actions-style CI.",
		OGType:          "website",
		StructuredData:  organizationStructuredData(publicBaseURL(h.baseURL, r)),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.render.RenderPage(w, r, "hello", data); err != nil {
		h.logger.Error("render hello", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
