// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlers(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Register(mux, Deps{
		Logger:      logger,
		TemplatesFS: testTemplatesFS(t),
		StaticFS:    testStaticFS(t),
		LogoSVG:     `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantBodyAny []string
		wantHeader  map[string]string
	}{
		{
			name:        "hello page",
			path:        "/",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{"shithub", "GitHub. Open source. Without Copilot.", "Sprint 00", `<meta name="description"`, `<link rel="canonical"`},
			wantHeader:  map[string]string{"Content-Type": "text/html; charset=utf-8"},
		},
		{
			name:        "about page",
			path:        "/about",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{"No hard feelings to GitHub", "AI training on my code", `<meta name="description"`},
			wantHeader:  map[string]string{"Content-Type": "text/html; charset=utf-8"},
		},
		{
			name:        "robots",
			path:        "/robots.txt",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{"User-agent: *", "Allow: /", "Disallow: /admin", "Sitemap: http://example.com/sitemap.xml"},
			wantHeader:  map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		},
		{
			name:        "sitemap",
			path:        "/sitemap.xml",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`, "<loc>http://example.com/</loc>", "<loc>http://example.com/about</loc>"},
			wantHeader:  map[string]string{"Content-Type": "application/xml; charset=utf-8"},
		},
		{
			name:        "healthz",
			path:        "/healthz",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{"ok"},
		},
		{
			name:        "readyz",
			path:        "/readyz",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{"ready"},
		},
		{
			name:        "logo svg",
			path:        "/static/logo/shithub.svg",
			wantStatus:  http.StatusOK,
			wantBodyAny: []string{"<svg", "shithub"},
		},
		{
			name:       "unknown route 404",
			path:       "/this-path-does-not-exist",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status: got %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tc.wantBodyAny {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q\nbody=%q", want, body)
				}
			}
			for k, want := range tc.wantHeader {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("header %s: got %q, want %q", k, got, want)
				}
			}
		})
	}
}

// TestHealthzHEAD pins SR2 L8: HEAD /healthz must return 200, not
// 405. Strict probes (some k8s livenessProbes, certain monitoring
// tools) issue HEAD-only requests; chi only registers the methods
// you ask for, so the GET-only registration would 405 HEAD probes.
func TestHealthzHEAD(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Register(mux, Deps{
		Logger:      logger,
		TemplatesFS: testTemplatesFS(t),
		StaticFS:    testStaticFS(t),
		LogoSVG:     `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /healthz: status %d, want 200", rec.Code)
	}
}
