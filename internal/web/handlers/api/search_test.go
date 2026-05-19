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

// G9a (F19): /search/issues result items must carry `html_url` and a
// nested `repository` envelope with at minimum `full_name`. Pre-fix
// `shithub status --json assigned_issues` rendered every row's
// repository and url as empty strings. This test pins the wire shape
// the top-level status dashboard depends on.
func TestSearch_IssuesIncludeRepositoryEnvelopeAndHTMLURL(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "lighthouse needle", "body": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/search/issues?q=lighthouse", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("search: %d %s", rr.Code, rr.Body.String())
	}
	// Decode the items as a loose map so we can probe the new keys
	// without coupling the test to handler-internal types.
	var generic struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &generic); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(generic.Items) == 0 {
		t.Fatalf("no items; body=%s", rr.Body.String())
	}
	row := generic.Items[0]
	if row["html_url"] == "" || row["html_url"] == nil {
		t.Errorf("html_url should be populated; row=%+v", row)
	}
	repo, _ := row["repository"].(map[string]any)
	if repo == nil {
		t.Fatalf("repository envelope missing; row=%+v", row)
	}
	if repo["full_name"] != "alice/demo" {
		t.Errorf("repository.full_name: got %v, want alice/demo", repo["full_name"])
	}
	// `private` is bool false for the public demo repo — must be the
	// key present (not omitempty) so gh-compat decoders find it.
	if _, ok := repo["private"]; !ok {
		t.Errorf("repository.private key missing; repo=%+v", repo)
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

// G11 (F49): when the user supplies a non-empty query but every token
// strips out of the FTS lexer (single-char like "F", or hyphen-split
// like "F-audit" with single-char halves), the server must 422 with a
// user-actionable message instead of silently returning 0 results.
// Pre-fix the search command was indistinguishable from a no-match
// search; the CLI status line was just "no results found."
func TestSearch_FTSStrippedQueryReturns422(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	cases := []string{
		"/api/v1/search/repositories?q=F",
		"/api/v1/search/issues?q=F",
		"/api/v1/search/code?q=F",
	}
	for _, path := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: got %d, want 422; body=%s", path, rr.Code, rr.Body.String())
			continue
		}
		if !bytes.Contains(rr.Body.Bytes(), []byte("FTS-indexable")) {
			t.Errorf("%s: body should explain stripping; got %s", path, rr.Body.String())
		}
	}
}

// G11 (F49) boundary: a query that survives stripping ("audit" is a
// real word the english stemmer keeps) still 200s — we don't want the
// new pre-flight to false-positive on legitimate single-word queries.
func TestSearch_FTSValidLexemeStillSucceeds(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/issues?q=audit", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
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
