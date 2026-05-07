// SPDX-License-Identifier: AGPL-3.0-or-later

// Package handlers registers HTTP handlers on the web server's mux.
//
// S02 ships the full chi-routed surface plus error pages. Each future
// sprint adds its own routes via this package.
package handlers

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/tenseleyFlow/shithub/internal/auth/session"
	"github.com/tenseleyFlow/shithub/internal/web/middleware"
	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// Deps holds the dependencies the handlers need. The web package owns the
// embedded filesystems and constructs Deps; this package stays decoupled
// from the embed.FS instances so it remains testable.
type Deps struct {
	Logger       *slog.Logger
	TemplatesFS  fs.FS
	StaticFS     fs.FS
	LogoSVG      string
	SessionStore session.Store
	// ReadyCheck is optionally invoked by /readyz. Returning a non-nil
	// error makes /readyz report 503. If nil, /readyz always reports ready.
	ReadyCheck func(context.Context) error
	// MetricsHandler, when non-nil, is mounted at /metrics. Caller is
	// responsible for any access control (e.g. HTTP Basic auth wrapping).
	MetricsHandler http.Handler
}

// panicHandler implements middleware.PanicHandler. The recover middleware
// invokes it when a downstream handler panics; we render the styled 500
// page through the registered renderer.
type panicHandler struct {
	render *render.Renderer
}

func (h *panicHandler) HandlePanic(w http.ResponseWriter, r *http.Request, _ string, _ any) {
	h.render.HTTPError(w, r, http.StatusInternalServerError, "")
}

// RegisterChi wires every S02 route into r. Returns the chi.Router (for
// further wiring), a panic handler that the caller installs in the
// recover middleware, and a NotFound handler for the catch-all.
func RegisterChi(r *chi.Mux, deps Deps) (*chi.Mux, middleware.PanicHandler, http.HandlerFunc, error) {
	if deps.Logger == nil {
		return nil, nil, nil, fmt.Errorf("handlers.RegisterChi: nil Logger")
	}
	if deps.TemplatesFS == nil {
		return nil, nil, nil, fmt.Errorf("handlers.RegisterChi: nil TemplatesFS")
	}
	if deps.StaticFS == nil {
		return nil, nil, nil, fmt.Errorf("handlers.RegisterChi: nil StaticFS")
	}

	rr, err := render.New(deps.TemplatesFS, render.Options{
		Octicons: render.BuiltinOcticons(),
	})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("renderer: %w", err)
	}

	csrf := middleware.CSRF(middleware.CSRFConfig{
		Secure: false, // S37 enables under TLS
		FailureHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rr.HTTPError(w, r, http.StatusForbidden, "csrf")
		}),
	})

	// Static and health endpoints are CSRF-exempt; everything else passes
	// through the CSRF wrapper for state-changing methods.
	r.Group(func(r chi.Router) {
		r.Handle("/static/*", http.StripPrefix("/static/", staticFileServer(deps.StaticFS)))
		r.Get("/healthz", healthz)
		r.Handle("/readyz", readinessHandler(deps.ReadyCheck, deps.Logger))
		if deps.MetricsHandler != nil {
			r.Handle("/metrics", deps.MetricsHandler)
		}
	})

	// Application routes — CSRF protected.
	r.Group(func(r chi.Router) {
		r.Use(csrf)
		r.Get("/", helloHandler{render: rr, logoSVG: deps.LogoSVG, logger: deps.Logger}.ServeHTTP)
		// /internal/panic is a dev affordance: GET it to trigger the
		// panic-recovery path so an operator can confirm the styled 500
		// page renders. S35 will gate this behind a dev flag.
		r.Get("/internal/panic", panicTrigger)
	})

	notFound := func(w http.ResponseWriter, r *http.Request) {
		rr.HTTPError(w, r, http.StatusNotFound, r.URL.Path)
	}

	return r, &panicHandler{render: rr}, notFound, nil
}

// Register is preserved for the existing test suite that exercises the
// surface without bringing up the full server. Internally it wraps
// RegisterChi and mounts the chi router on mux.
func Register(mux *http.ServeMux, deps Deps) error {
	r := chi.NewRouter()
	_, _, notFound, err := RegisterChi(r, deps)
	if err != nil {
		return err
	}
	r.NotFound(notFound)
	mux.Handle("/", r)
	return nil
}

// panicTrigger panics on demand to exercise the recover middleware.
func panicTrigger(_ http.ResponseWriter, _ *http.Request) {
	panic("S02 panic trigger: this is intentional")
}
