// SPDX-License-Identifier: AGPL-3.0-or-later

package handlers

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnonGitShed_RunsAheadOfHTMLRateLimit pins the wiring order in
// the application group. The shed gate has to come first: a request
// it rejects must not reach the rate limiter, whose counter is a
// Postgres write. It also has to stay off the routes the rate limiter
// stays off (static, probes).
func TestAnonGitShed_RunsAheadOfHTMLRateLimit(t *testing.T) {
	t.Parallel()

	var order []string
	tag := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	mux := http.NewServeMux()
	if err := Register(mux, Deps{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		TemplatesFS:   testTemplatesFS(t),
		StaticFS:      testStaticFS(t),
		LogoSVG:       `<svg xmlns="http://www.w3.org/2000/svg"><title>shithub</title></svg>`,
		AnonGitShed:   tag("shed"),
		HTMLRateLimit: tag("ratelimit"),
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	rw := httptest.NewRecorder()
	mux.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, "/", nil))
	if rw.Code != http.StatusOK {
		t.Fatalf("GET /: status %d, want 200", rw.Code)
	}
	if len(order) != 2 || order[0] != "shed" || order[1] != "ratelimit" {
		t.Fatalf("middleware order = %v, want [shed ratelimit]", order)
	}

	for _, path := range []string{"/healthz", "/static/logo/shithub.svg", "/robots.txt"} {
		order = nil
		rw := httptest.NewRecorder()
		mux.ServeHTTP(rw, httptest.NewRequest(http.MethodGet, path, nil))
		if rw.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", path, rw.Code)
		}
		if len(order) != 0 {
			t.Errorf("GET %s ran %v, want neither middleware", path, order)
		}
	}
}
