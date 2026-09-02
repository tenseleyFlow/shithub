// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestHTMLRateLimit_AppliesToApplicationGroupOnly verifies the F02
// wiring: the HTMLRateLimit middleware fronts the CSRF-protected
// (application) group but stays out of /static, /healthz, /readyz,
// /metrics, /robots.txt, /sitemap.xml, and any mounter that lives
// on the CSRF-exempt group (API, avatars, billing webhook, notif
// public unsubscribe).
//
// We don't need a real ratelimit.Limiter here — the test stubs the
// middleware with a counter that 429s every call. If the stub
// fires on a route that should be excluded, the test fails.
func TestHTMLRateLimit_AppliesToApplicationGroupOnly(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	stub := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("X-HTMLRateLimit-Hit", "true")
			next.ServeHTTP(w, r)
		})
	}

	// Synthetic API + webhook mounters live on the CSRF-exempt
	// group; they must not see the HTMLRateLimit middleware fire.
	apiMounter := func(r chi.Router) {
		r.Get("/api/v1/__probe", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("api"))
		})
	}
	webhookMounter := func(r chi.Router) {
		r.Post("/stripe/webhook", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("webhook"))
		})
	}

	mux := http.NewServeMux()
	if err := Register(mux, Deps{
		Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		TemplatesFS:           testTemplatesFS(t),
		StaticFS:              testStaticFS(t),
		LogoSVG:               `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
		APIMounter:            apiMounter,
		BillingWebhookMounter: webhookMounter,
		HTMLRateLimit:         stub,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	gated := []struct {
		name string
		path string
	}{
		{"hello page", "/"},
		{"about page", "/about"},
	}
	excluded := []struct {
		name           string
		method         string
		path           string
		wantStatusList []int
	}{
		{"static asset", http.MethodGet, "/static/logo/shithub.svg", []int{http.StatusOK}},
		{"robots", http.MethodGet, "/robots.txt", []int{http.StatusOK}},
		{"sitemap", http.MethodGet, "/sitemap.xml", []int{http.StatusOK}},
		{"healthz GET", http.MethodGet, "/healthz", []int{http.StatusOK}},
		{"healthz HEAD", http.MethodHead, "/healthz", []int{http.StatusOK}},
		{"readyz", http.MethodGet, "/readyz", []int{http.StatusOK}},
		{"api probe", http.MethodGet, "/api/v1/__probe", []int{http.StatusOK}},
		{"stripe webhook", http.MethodPost, "/stripe/webhook", []int{http.StatusOK}},
	}

	// Each gated route must run through the stub middleware.
	for _, tc := range gated {
		tc := tc
		t.Run("gated:"+tc.name, func(t *testing.T) {
			before := hits.Load()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rw := httptest.NewRecorder()
			mux.ServeHTTP(rw, req)
			if rw.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rw.Code)
			}
			if got := hits.Load() - before; got != 1 {
				t.Errorf("middleware hits for %s: got %d, want 1", tc.path, got)
			}
			if rw.Header().Get("X-HTMLRateLimit-Hit") != "true" {
				t.Errorf("X-HTMLRateLimit-Hit not stamped for %s", tc.path)
			}
		})
	}

	// Each excluded route must NOT trigger the middleware.
	for _, tc := range excluded {
		tc := tc
		t.Run("excluded:"+tc.name, func(t *testing.T) {
			before := hits.Load()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rw := httptest.NewRecorder()
			mux.ServeHTTP(rw, req)
			matched := false
			for _, want := range tc.wantStatusList {
				if rw.Code == want {
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("status = %d, want one of %v", rw.Code, tc.wantStatusList)
			}
			if got := hits.Load() - before; got != 0 {
				t.Errorf("middleware fired on %s: hits delta = %d, want 0", tc.path, got)
			}
			if rw.Header().Get("X-HTMLRateLimit-Hit") != "" {
				t.Errorf("X-HTMLRateLimit-Hit stamped on excluded route %s", tc.path)
			}
		})
	}
}

// TestHTMLRateLimit_NilMiddlewareIsNoop confirms that the existing
// test setups (which don't plumb HTMLRateLimit) still work after
// the wiring lands. The application group must serve normally and
// the field-is-nil branch must not panic.
func TestHTMLRateLimit_NilMiddlewareIsNoop(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	if err := Register(mux, Deps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		TemplatesFS: testTemplatesFS(t),
		StaticFS:    testStaticFS(t),
		LogoSVG:     `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("hello with nil HTMLRateLimit: status=%d, want 200", rw.Code)
	}
}

// TestHealthz_NeverThrottled pins the availability contract the DO
// uptime check relies on: even with a limiter that refuses every
// request it is given, /healthz answers 200. The probe targets
// /healthz precisely because a throttled probe reads as an outage
// (see docs/internal/runbooks/alerts.md).
//
// Unlike the wiring test above — which stubs a pass-through and
// counts hits — this one hard-fails the request, so a future edit
// that moves /healthz inside the limiter group turns the assertion
// red rather than merely changing a counter.
func TestHealthz_NeverThrottled(t *testing.T) {
	t.Parallel()

	denyAll := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
	}

	mux := http.NewServeMux()
	if err := Register(mux, Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		TemplatesFS:   testTemplatesFS(t),
		StaticFS:      testStaticFS(t),
		LogoSVG:       `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
		HTMLRateLimit: denyAll,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Sanity: the limiter really is refusing the application group.
	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusTooManyRequests {
		t.Fatalf("gated route: status=%d, want 429 (deny-all limiter not wired)", rw.Code)
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rw := httptest.NewRecorder()
		mux.ServeHTTP(rw, httptest.NewRequest(method, "/healthz", nil))
		if rw.Code != http.StatusOK {
			t.Errorf("%s /healthz: status=%d, want 200", method, rw.Code)
		}
	}
}
