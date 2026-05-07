// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"html/template"
	"log/slog"
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/version"
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
}

func (h helloHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data := helloData{
		Title:   "Welcome",
		Version: version.Version,
		Commit:  version.Commit,
		BuiltAt: version.BuiltAt,
		LogoSVG: template.HTML(h.logoSVG), // #nosec G203 — embedded server-owned asset
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.render.Render(w, "hello", data); err != nil {
		h.logger.Error("render hello", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
}
