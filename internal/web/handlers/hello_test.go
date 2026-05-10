// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/web/render"
)

// TestHelloHandler_RendersAgainstRealTemplates exercises the homepage
// handler against the real templates on disk (NOT the embed.FS, since
// importing internal/web from internal/web/handlers would cycle). The
// repo layout makes this safe: tests run with cwd = package dir, so
// os.DirFS("../templates") reads internal/web/templates/.
//
// This is a regression for an outage on 2026-05-09: _nav.html started
// referencing .GlobalSearchQuery and the typed helloData struct didn't
// carry the field, so every request to / returned 500 from a template
// execute error. html/template parses fine — the failure only shows
// at exec time, and only when the data is a typed struct (maps swallow
// missing keys under `with`). Hence: render the actual handler with
// the actual struct against the actual templates.
func TestHelloHandler_RendersAgainstRealTemplates(t *testing.T) {
	t.Parallel()

	tmplFS := os.DirFS("../templates")
	r, err := render.New(tmplFS, render.Options{})
	if err != nil {
		t.Fatalf("render.New on real templates: %v", err)
	}

	logo, err := os.ReadFile("../static/logo/shithub.svg")
	if err != nil {
		t.Fatalf("read logo: %v", err)
	}

	h := helloHandler{
		render:  r,
		logoSVG: string(logo),
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	req := httptest.NewRequest("GET", "/", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)

	if rw.Code != 200 {
		t.Fatalf("expected 200, got %d; body=%s", rw.Code, rw.Body.String())
	}
}
