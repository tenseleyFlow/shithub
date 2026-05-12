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

type apiSubscription struct {
	Level    string `json:"level"`
	Explicit bool   `json:"explicit"`
}

func putSubscription(t *testing.T, router http.Handler, token, owner, repo, level string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"level": level})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/"+owner+"/"+repo+"/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put subscription status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestWatching_GetDefaultsToImplicit(t *testing.T) {
	// alice owns alice/demo. With no explicit watch row, the viewer
	// gets the implicit `participating` default.
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiSubscription
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Level != "participating" || got.Explicit {
		t.Errorf("expected implicit participating; got %+v", got)
	}
}

func TestWatching_PutThenGetReturnsExplicit(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	putSubscription(t, router, token, "alice", "demo", "all")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got apiSubscription
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Level != "all" || !got.Explicit {
		t.Errorf("expected explicit all; got %+v", got)
	}
}

func TestWatching_PutRejectsBadLevel(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]any{"level": "godmode"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/subscription", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestWatching_DeleteRevertsToImplicit(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	putSubscription(t, router, token, "alice", "demo", "all")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// follow-up GET shows implicit again
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscription", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got apiSubscription
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Explicit {
		t.Errorf("expected implicit after delete; got %+v", got)
	}
}

func TestWatching_SubscribersList(t *testing.T) {
	// alice (owner) auto-watches on collab; bob explicitly watches at
	// `all`. List should surface both.
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	putSubscription(t, router, bobToken, "alice", "demo", "all")

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

func TestWatching_IgnoreExcludedFromSubscribers(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead), string(pat.ScopeUserWrite))

	putSubscription(t, router, bobToken, "alice", "demo", "ignore")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/subscribers", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
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

	body, _ := json.Marshal(map[string]any{"level": "all"})
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
