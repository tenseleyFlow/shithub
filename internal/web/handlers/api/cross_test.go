// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/version"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

// newCrossCuttingAPIRouter builds the smallest /api/v1 router we can —
// no runner JWT, no secret box, no object store. Enough to exercise the
// PATAuth + RequireScope + apilimit + meta surface.
func newCrossCuttingAPIRouter(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RateLimiter: ratelimit.New(pool),
		BaseURL:     "https://shithub.test",
		APILimit: apilimit.Config{
			AuthedPerHour: 5000,
			AnonPerHour:   60,
			Logger:        logger,
		},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

func crossCuttingUser(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	user, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: runnerAPIFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return user.ID
}

func TestCrossCutting_AuthFailureReturnsJSONEnvelope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json prefix", ct)
	}
	if wa := rr.Header().Get("WWW-Authenticate"); !strings.Contains(wa, "Bearer") {
		t.Errorf("WWW-Authenticate missing Bearer challenge: %q", wa)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rr.Body.String())
	}
	if envelope.Error == "" {
		t.Errorf("error envelope empty: %s", rr.Body.String())
	}
}

func TestCrossCutting_ScopeRejectReturnsJSONEnvelope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	// User has only user:read; /api/v1/user needs user:read, so to
	// exercise a scope reject we use a different route that requires
	// repo:write. We don't have a repo wired here, so we forge a path
	// the scope-decorator wrapped around check-runs, knowing it will
	// short-circuit on scope before policy resolution. The scope check
	// runs before the resolveAPIRepo call, so we get 403 directly.
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/check-runs", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json prefix", ct)
	}
	if want := string(pat.ScopeRepoWrite); rr.Header().Get("X-Accepted-OAuth-Scopes") != want {
		t.Errorf("X-Accepted-OAuth-Scopes: got %q, want %q", rr.Header().Get("X-Accepted-OAuth-Scopes"), want)
	}
	if got := rr.Header().Get("X-OAuth-Scopes"); got != string(pat.ScopeUserRead) {
		t.Errorf("X-OAuth-Scopes: got %q, want %q", got, pat.ScopeUserRead)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rr.Body.String())
	}
	if !strings.Contains(envelope.Error, "scope") {
		t.Errorf("error envelope: got %q, want one mentioning scope", envelope.Error)
	}
}

func TestCrossCutting_XOAuthScopesOnSuccess(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-OAuth-Scopes"); got != string(pat.ScopeUserRead) {
		t.Errorf("X-OAuth-Scopes: got %q, want %q", got, pat.ScopeUserRead)
	}
}

func TestCrossCutting_RateLimitHeadersStampedAuthed(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.RemoteAddr = "10.0.0.5:12345"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "5000" {
		t.Errorf("X-RateLimit-Limit: got %q, want 5000", got)
	}
	if rr.Header().Get("X-RateLimit-Remaining") == "" {
		t.Errorf("X-RateLimit-Remaining missing")
	}
	if rr.Header().Get("X-RateLimit-Reset") == "" {
		t.Errorf("X-RateLimit-Reset missing")
	}
}

func TestCrossCutting_RateLimitHeadersStampedAnon(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	req.RemoteAddr = "10.0.0.6:54321"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-RateLimit-Limit"); got != "60" {
		t.Errorf("X-RateLimit-Limit (anon): got %q, want 60", got)
	}
}

func TestCrossCutting_MetaPayload(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Version      string   `json:"version"`
		Commit       string   `json:"commit"`
		BuiltAt      string   `json:"built_at"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode meta: %v; body=%s", err, rr.Body.String())
	}
	if resp.Version != version.Version {
		t.Errorf("version: got %q, want %q", resp.Version, version.Version)
	}
	if len(resp.Capabilities) == 0 {
		t.Errorf("capabilities empty: %#v", resp)
	}
	if !containsString(resp.Capabilities, "pat-auth") {
		t.Errorf("capabilities missing pat-auth: %v", resp.Capabilities)
	}
}

func TestCrossCutting_RateLimitDeniedJSON(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Tiny limits so we can trigger the deny path without 5001 requests.
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RateLimiter: ratelimit.New(pool),
		BaseURL:     "https://shithub.test",
		APILimit: apilimit.Config{
			AuthedPerHour: 1,
			AnonPerHour:   1,
			Logger:        logger,
		},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	router := chi.NewRouter()
	h.Mount(router)

	// First anon request consumes the entire budget.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	req.RemoteAddr = "10.0.0.99:11111"
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("first call status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	// Second request exceeds the budget.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	req.RemoteAddr = "10.0.0.99:11112"
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status: got %d, want 429; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json prefix", ct)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After missing on 429")
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode 429 envelope: %v; body=%s", err, rr.Body.String())
	}
	if !strings.Contains(envelope.Error, "rate") {
		t.Errorf("429 error: got %q", envelope.Error)
	}
}

// TestCrossCutting_UsersByName pins the S60 audit-finding A5
// endpoint: GET /api/v1/users/{username} returns the GitHub-compat
// public-profile envelope. user:read scope required.
func TestCrossCutting_UsersByName(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		ID      int64  `json:"id"`
		Login   string `json:"login"`
		Type    string `json:"type"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if resp.Login != "alice" || resp.Type != "User" {
		t.Errorf("shape: %+v", resp)
	}
	if resp.ID != userID {
		t.Errorf("ID: got %d, want %d", resp.ID, userID)
	}
	if resp.HTMLURL == "" {
		t.Error("html_url should be populated")
	}
}

func TestCrossCutting_UsersByName_Missing(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/does-not-exist", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404", rr.Code)
	}
}
