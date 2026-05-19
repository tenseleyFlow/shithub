// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
