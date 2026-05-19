// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

// G9b (F29): `GET /api/v1/issues` user-scoped endpoint.
//
//	filter=assigned → issues where the auth user is in issue_assignees
//	filter=created  → issues where author_user_id matches
//	filter=mentioned → 422 (mention index not yet built)
//
// Pre-G9b the route returned 404 and both `shithub pr status` and
// `shithub issue status` were broken end-to-end. The test pins
// happy paths, the mentioned-422, and the bogus-filter rejection.
func TestIssuesAcrossRepos_AssignedFilter(t *testing.T) {
	pool, router, aliceID, _, aliceTok := seedIssuesEnv(t, "alice")
	// Bob has repo:write on alice/demo (via the shared user fixtures —
	// seedRepoCreatorUser just inserts a user row), so he can be
	// assigned but isn't a collaborator. Make him a collaborator so
	// his issue is visible to alice (or seed alice's own issue and
	// assign her to it for the simpler case).
	bobID := seedRepoCreatorUser(t, pool, "bob")
	_ = bobID

	// Alice creates an issue and assigns herself.
	body, _ := json.Marshal(map[string]any{
		"title":     "self-assigned",
		"assignees": []string{"alice"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+aliceTok)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed issue: %d %s", rr.Code, rr.Body.String())
	}

	// GET /api/v1/issues should return that issue under filter=assigned.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/issues?filter=assigned", nil)
	req.Header.Set("Authorization", "Bearer "+aliceTok)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list assigned: %d %s", rr.Code, rr.Body.String())
	}
	var rows []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %v", len(rows), rows)
	}
	repo, _ := rows[0]["repository"].(map[string]any)
	if repo == nil || repo["full_name"] != "alice/demo" {
		t.Errorf("repository.full_name: %+v", repo)
	}
	if rows[0]["html_url"] == nil || rows[0]["html_url"] == "" {
		t.Errorf("html_url should be populated: %+v", rows[0])
	}
	_ = aliceID
}

// G9b (F29): filter=created selects issues whose author matches the
// auth user. Distinct from assigned: bob's authored issue lands here
// but not on alice's assigned list.
func TestIssuesAcrossRepos_CreatedFilter(t *testing.T) {
	pool, router, _, _, aliceTok := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobTok := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	// Bob opens an issue on the public alice/demo repo. (issue:create
	// is allowed on a public repo for any logged-in user.)
	body, _ := json.Marshal(map[string]any{"title": "bob's issue"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bobTok)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("bob seed: %d %s", rr.Code, rr.Body.String())
	}

	// Bob's /issues?filter=created lists it.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/issues?filter=created", nil)
	req.Header.Set("Authorization", "Bearer "+bobTok)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list created: %d %s", rr.Code, rr.Body.String())
	}
	var rows []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &rows)
	if len(rows) != 1 {
		t.Fatalf("bob's created list: want 1, got %d", len(rows))
	}

	// Alice's /issues?filter=created should NOT contain bob's issue
	// (different author).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/issues?filter=created", nil)
	req.Header.Set("Authorization", "Bearer "+aliceTok)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var aliceRows []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &aliceRows)
	if len(aliceRows) != 0 {
		t.Errorf("alice's created should be empty: %v", aliceRows)
	}
}

// G9b (F29): mentioned filter explicitly 422s with a clear "not yet
// supported" message — pre-G9b the CLI's `pr status` mentioned-section
// got "shithub: not found", now it gets a typed warning.
func TestIssuesAcrossRepos_MentionedRejectedWith422(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?filter=mentioned", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("mentioned: code=%d want 422; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "mentioned is not yet supported") {
		t.Errorf("expected 'mentioned is not yet supported' in body: %s", rr.Body.String())
	}
}

// G9b boundary: unknown filter values 422 with a list of accepted
// names so the CLI can surface a clear error.
func TestIssuesAcrossRepos_RejectsBogusFilter(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?filter=banana", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("bogus filter: code=%d want 422", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "assigned, created, mentioned") {
		t.Errorf("body should list valid filters: %s", rr.Body.String())
	}
}

// G9b boundary: state must be open/closed/all; pre-fix invalid state
// would have been silently dropped (search-style). Mirrors G5's
// strict-state rules across all list endpoints.
func TestIssuesAcrossRepos_RejectsBogusState(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/issues?state=banana", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("bogus state: code=%d want 422", rr.Code)
	}
}
