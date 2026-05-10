// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// /metrics MUST be served uncompressed even when the scraper advertises
// gzip support. Alloy 1.16 (and several other prom-compatible scrapers)
// mis-handle Content-Encoding: gzip and parse the raw 0x1f magic byte
// as text, failing the scrape silently with up=0.
func TestMetricsServedUncompressedWithGzipAccept(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, "# HELP test_metric A test metric\n# TYPE test_metric counter\ntest_metric 1\n")
	})

	r := chi.NewRouter()
	if _, _, _, err := RegisterChi(r, Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		TemplatesFS:    testTemplatesFS(t),
		StaticFS:       testStaticFS(t),
		LogoSVG:        `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
		MetricsHandler: handler,
	}); err != nil {
		t.Fatalf("RegisterChi: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (Prometheus scrapers expect plain text)", enc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "test_metric 1") {
		t.Errorf("body missing metric text; got %q", body)
	}
	if strings.HasPrefix(body, "\x1f\x8b") {
		t.Errorf("body starts with gzip magic bytes — middleware compressed /metrics")
	}
}
