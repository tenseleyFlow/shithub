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

type searchEnvelope struct {
	TotalCount        int64           `json:"total_count"`
	IncompleteResults bool            `json:"incomplete_results"`
	Items             json.RawMessage `json:"items"`
}

func TestSearch_RepositoriesEmptyQueryReturns422(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/repositories?q=", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSearch_RepositoriesFindsByName(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/repositories?q=demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var env searchEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.TotalCount < 1 {
		t.Errorf("expected at least one hit; got %+v", env)
	}
	if env.IncompleteResults {
		t.Errorf("incomplete_results should be false in v1; got true")
	}
	// Confirm items decode into our shape and the alice/demo row is there.
	var items []struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
	}
	if err := json.Unmarshal(env.Items, &items); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	found := false
	for _, it := range items {
		if it.FullName == "alice/demo" {
			found = true
		}
	}
	if !found {
		t.Errorf("alice/demo not in results: %+v", items)
	}
}

func TestSearch_IssuesByTitle(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	// Seed one issue so the search has something to find.
	body, _ := json.Marshal(map[string]any{"title": "needle in haystack", "body": "first cut"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/search/issues?q=needle", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var env searchEnvelope
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.TotalCount < 1 {
		t.Errorf("expected at least one hit; got %+v", env)
	}
}

func TestSearch_IssuesTypeFilterValidates(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/issues?q=anything&type=banana", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSearch_AnonymousAllowed(t *testing.T) {
	_, router, _, _, _ := seedIssuesEnv(t, "alice")

	// No Authorization header — search must still succeed (visibility
	// filter inside the search package restricts to public).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/repositories?q=demo", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("anon search: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSearch_ScopeRejectOnAuthedTokenWithoutRepoRead(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	adminOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeAdminRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/repositories?q=demo", nil)
	req.Header.Set("Authorization", "Bearer "+adminOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
