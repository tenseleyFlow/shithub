// SPDX-License-Identifier: AGPL-3.0-or-later

// Package handlers registers HTTP handlers on the web server's mux.
//
// S00 ships only the hello page, static asset server, and health endpoints.
// Each future sprint adds its own routes via this package.
package handlers

import (
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps holds the dependencies the handlers need. The web package owns the
// embedded filesystems and constructs Deps; this package stays decoupled
// from the embed.FS instances so it remains testable.
type Deps struct {
	Logger      *slog.Logger
	TemplatesFS fs.FS
	StaticFS    fs.FS
	LogoSVG     string
}

// Register wires every S00 route into mux. Later sprints' Register entrypoints
// are called from here; for S00 the surface is small.
func Register(mux *http.ServeMux, deps Deps) error {
	if deps.Logger == nil {
		return fmt.Errorf("handlers.Register: nil Logger")
	}
	if deps.TemplatesFS == nil {
		return fmt.Errorf("handlers.Register: nil TemplatesFS")
	}
	if deps.StaticFS == nil {
		return fmt.Errorf("handlers.Register: nil StaticFS")
	}

	r, err := render.New(deps.TemplatesFS)
	if err != nil {
		return fmt.Errorf("renderer: %w", err)
	}

	mux.Handle("GET /static/", http.StripPrefix("/static/", staticFileServer(deps.StaticFS)))
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /readyz", readyz)
	mux.Handle("GET /{$}", helloHandler{render: r, logoSVG: deps.LogoSVG, logger: deps.Logger})

	return nil
}
