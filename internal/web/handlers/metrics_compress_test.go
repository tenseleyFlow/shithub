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

	"github.com/tenseleyFlow/shithub/internal/infra/metrics"
)

// /metrics MUST be served uncompressed even when the scraper advertises
// gzip support. Alloy 1.16 (and several other prom-compatible scrapers)
// mis-handle Content-Encoding: gzip and parse the raw 0x1f magic byte
// as text, failing the scrape silently with up=0.
//
// Two layers can produce gzip on this route:
//  1. The chi Compress middleware in the public route group.
//  2. promhttp's own DisableCompression knob (default false).
//
// Each test below pins one layer so a regression in either fires loud.

// Layer 1: the route is mounted outside the Compress middleware group.
func TestMetricsRouteBypassesCompressMiddleware(t *testing.T) {
	t.Parallel()

	// Stub handler — the test is about the middleware path, not the
	// real /metrics body shape.
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
	if strings.HasPrefix(rec.Body.String(), "\x1f\x8b") {
		t.Error("body starts with gzip magic bytes — middleware compressed /metrics")
	}
}

// Layer 2: the real metrics.Handler() must have promhttp's compression
// disabled. Without DisableCompression: true on HandlerOpts, promhttp
// gzips when the client advertises Accept-Encoding: gzip — entirely
// independent of our middleware. The post-hardening audit caught this:
// the layer-1 test passed but the live droplet still emitted gzip.
func TestMetricsHandlerPromhttpCompressionDisabled(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	metrics.Handler("", "").ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("Content-Encoding = %q, want empty (DisableCompression must be set on promhttp.HandlerOpts)", enc)
	}
	if strings.HasPrefix(rec.Body.String(), "\x1f\x8b") {
		t.Error("body starts with gzip magic bytes — promhttp compressed despite DisableCompression intent")
	}
}
