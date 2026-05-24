// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// testCORSPolicy returns the policy used by the test mux: same-host
// = https://example.com (matches the existing test fixtures); explicit
// allow-list = {https://allowed.example.com} for I11 regression
// coverage. Unknown origins (https://attacker.example.com, `null`,
// `*`) fall through to the no-ACAO branch.
func testCORSPolicy() corsOriginPolicy {
	return corsOriginPolicy{
		sameHost: "https://example.com",
		allowed: map[string]struct{}{
			"https://allowed.example.com": {},
		},
	}
}

// TestMethodNotAllowed_APIRouteEmitsJSONEnvelope pins F2-11 / F2-17:
// when a /api/v1/* route exists but the method isn't registered, the
// 405 response must carry a `{"error":...}` body so callers piping the
// response into `jq` or matching `.error` see a real message.
// Pre-fix chi's default 405 sent an empty body and the CLI surfaced
// "shithub API: 405 (no message)".
func TestMethodNotAllowed_APIRouteEmitsJSONEnvelope(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("NOPE", "/api/v1/user", nil)
	methodNotAllowedHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type: got %q want application/json*", got)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if body["error"] == "" {
		t.Errorf("error message missing: %+v", body)
	}
	if !strings.Contains(body["error"], "NOPE") {
		t.Errorf("error should reference method: %q", body["error"])
	}
}

// TestMethodNotAllowed_EmitsAllowHeader pins H19: every 405 must
// include the RFC 9110 §15.5.6 `Allow:` header. The handler probes
// the mux to discover which methods are registered on the route, so
// the header reflects the actual ServerSurface and not a fixed list.
func TestMethodNotAllowed_EmitsAllowHeader(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.Post("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/v1/user", nil)
	mx.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", rr.Code)
	}
	got := rr.Header().Get("Allow")
	for _, want := range []string{"GET", "POST", "OPTIONS"} {
		if !strings.Contains(got, want) {
			t.Errorf("Allow header missing %q: %q", want, got)
		}
	}
}

// TestMethodNotAllowed_CORSPreflight pins H20: an OPTIONS preflight
// against an API endpoint that exists for other methods must answer
// 204 with the standard CORS headers so a browser-based client can
// proceed. Non-API OPTIONS keep the default 405 (HTML pages are
// session-cookie, not CORS-served).
func TestMethodNotAllowed_CORSPreflight(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/api/v1/user", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	mx.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("ACAO: got %q want https://example.com", got)
	}
	if got := rr.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Errorf("ACAM: got %q want includes GET", got)
	}
	if got := rr.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary: got %q want Origin", got)
	}
}

func TestMethodNotAllowed_CORSPreflightNonAPI(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/login", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/login", nil)
	req.Header.Set("Origin", "https://example.com")
	mx.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("non-API OPTIONS should keep 405; got %d", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("non-API path must not emit CORS headers")
	}
}

// TestMethodNotAllowed_NonAPIPathKeepsBareResponse pins the boundary:
// HTML pages have their own 405 rendering elsewhere (middleware /
// renderer), so the global handler must not impose a JSON body on
// non-API paths.
func TestMethodNotAllowed_NonAPIPathKeepsBareResponse(t *testing.T) {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", nil)
	methodNotAllowedHandler(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d want 405", rr.Code)
	}
	if got := rr.Header().Get("Content-Type"); strings.HasPrefix(got, "application/json") {
		t.Errorf("non-API path should not emit JSON; got Content-Type %q", got)
	}
}

// TestCORSPreflight_RejectsUnknownOrigin pins audit-I33: pre-fix the
// handler echoed any Origin header into ACAO. Now unknown origins
// (`https://attacker.example.com`, `null`, `*`) get a 204 with no
// ACAO header, which the browser treats as "server doesn't speak
// CORS for this origin" and refuses the cross-origin request.
func TestCORSPreflight_RejectsUnknownOrigin(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	for _, origin := range []string{"https://attacker.example.com", "null", "*"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/api/v1/user", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		mx.ServeHTTP(rr, req)

		if rr.Code != http.StatusNoContent {
			t.Errorf("origin=%q: status got %d want 204", origin, rr.Code)
		}
		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("origin=%q: ACAO leak %q (pre-fix this echoed back)", origin, got)
		}
	}
}

// TestCORSPreflight_AllowsSameHost pins audit-I33: same-host origin
// (matches the configured sameHost) gets the full preflight 204 with
// ACAO echoing the origin.
func TestCORSPreflight_AllowsSameHost(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/api/v1/user", nil)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	mx.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://example.com" {
		t.Errorf("same-host ACAO: got %q", got)
	}
}

// TestCORSPreflight_AllowsAllowlisted pins audit-I33: an explicit
// allow-list entry receives a real ACAO.
func TestCORSPreflight_AllowsAllowlisted(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("OPTIONS", "/api/v1/user", nil)
	req.Header.Set("Origin", "https://allowed.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	mx.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d want 204", rr.Code)
	}
	if got := rr.Header().Get("Access-Control-Allow-Origin"); got != "https://allowed.example.com" {
		t.Errorf("allow-listed ACAO: got %q", got)
	}
}

// TestCORSPreflight_AllowsLocalhostDev pins audit-I33: a CRA/Vite
// dev server on localhost gets through without operators needing to
// list every port they spin up.
func TestCORSPreflight_AllowsLocalhostDev(t *testing.T) {
	mx := chi.NewRouter()
	mx.Get("/api/v1/user", func(_ http.ResponseWriter, _ *http.Request) {})
	mx.MethodNotAllowed(methodNotAllowedHandlerFor(mx, testCORSPolicy()))

	for _, origin := range []string{"http://localhost:5173", "http://127.0.0.1:3000"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("OPTIONS", "/api/v1/user", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		mx.ServeHTTP(rr, req)

		if got := rr.Header().Get("Access-Control-Allow-Origin"); got != origin {
			t.Errorf("localhost origin=%q: ACAO got %q want echo", origin, got)
		}
	}
}
