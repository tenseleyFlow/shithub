// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

type apiSubscriber struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Level       string `json:"level"`
	UpdatedAt   string `json:"updated_at"`
}

// apiSubscription mirrors the gh-compat shape returned by
// GET/PUT /subscription. B5 audit decision: server adapts to gh's
// {subscribed, ignored} pair; the server's native WatchLevel enum
// (all|participating|ignore) is mapped at the handler boundary.
type apiSubscription struct {
	Subscribed    bool   `json:"subscribed"`
	Ignored       bool   `json:"ignored"`
	Reason        any    `json:"reason"`
	CreatedAt     string `json:"created_at"`
	URL           string `json:"url"`
	RepositoryURL string `json:"repository_url"`
}

// putGHSubscribed PUTs the gh-compat body {subscribed:true} and
// asserts a 200 response (the helper used by tests that need an
// explicit subscription as setup state).
func putGHSubscribed(t *testing.T, router http.Handler, token, owner, repo string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"subscribed": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/"+owner+"/"+repo+"/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put subscription status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// B5: GET with no explicit subscription returns 404 (gh-style). The
// implicit `participating` default has no REST representation —
// clients infer it from the absence.
func TestWatching_GetNoExplicitReturns404(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

// B5: PUT subscribed=true sets WatchAll, GET reflects {subscribed:true}.
func TestWatching_PutSubscribedThenGetReflectsState(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	putGHSubscribed(t, router, token, "alice", "demo")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET after PUT: %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiSubscription
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if !got.Subscribed || got.Ignored {
		t.Errorf("expected subscribed=true ignored=false; got %+v", got)
	}
	if got.URL == "" || got.RepositoryURL == "" {
		t.Errorf("URLs should echo the request path: %+v", got)
	}
}

// B5: PUT ignored=true sets WatchIgnore.
func TestWatching_PutIgnoredFlipsLevel(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]any{"ignored": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT ignored=true: %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiSubscription
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Subscribed || !got.Ignored {
		t.Errorf("expected subscribed=false ignored=true; got %+v", got)
	}
}

// B5: subscribed=true + ignored=true is contradictory — 422.
func TestWatching_PutBothTrueIs422(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]any{"subscribed": true, "ignored": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

// B5: PUT {} (both false) clears explicit subscription, returns 204.
func TestWatching_PutBothFalseClears(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	// First set subscribed=true, then clear with both-false.
	putGHSubscribed(t, router, token, "alice", "demo")

	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("PUT empty body clears: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// Follow-up GET shows 404 again.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("GET after clear: got %d, want 404", rr.Code)
	}
}

// B5: DELETE is idempotent. No prior row → still 204.
func TestWatching_DeleteIsIdempotent(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Errorf("first DELETE: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

// B5: subscribers list still reports an explicit `all` watcher.
// Verifies the gh-compat PUT path successfully writes the row that the
// subscribers GET surfaces.
func TestWatching_SubscribersList(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	putGHSubscribed(t, router, bobToken, "alice", "demo")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiSubscriber
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	var foundBob bool
	for _, s := range listed {
		if s.Username == "bob" && s.Level == "all" {
			foundBob = true
		}
	}
	if !foundBob {
		t.Errorf("expected bob with level=all in subscriber list: %+v", listed)
	}
}

// B5: ignore-level rows stay out of the subscribers list. Server-side
// filter still applies regardless of the gh-compat REST shape.
func TestWatching_IgnoreExcludedFromSubscribers(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]any{"ignored": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT ignored setup: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var listed []apiSubscriber
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	for _, s := range listed {
		if s.Username == "bob" {
			t.Errorf("ignore-level watch leaked into subscribers list: %+v", s)
		}
	}
}

func TestWatching_PutRequiresUserWriteScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"subscribed": true})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestWatching_GetRequiresRepoRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
