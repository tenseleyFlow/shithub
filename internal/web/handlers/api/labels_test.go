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

type apiLabel struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
}

func TestLabels_CreateListGetUpdateDelete(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	// Default-seeded labels exist; list should already be non-empty.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/labels", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("initial list: %d", rr.Code)
	}
	var initial []apiLabel
	if err := json.Unmarshal(rr.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial: %v", err)
	}
	if len(initial) == 0 {
		t.Fatalf("expected seeded default labels; got empty list")
	}

	// Create a new label.
	body, _ := json.Marshal(map[string]any{"name": "needs-triage", "color": "ff00aa", "description": "triage me"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiLabel
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Name != "needs-triage" || created.Color != "ff00aa" {
		t.Errorf("shape: %+v", created)
	}

	// Get by name.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/labels/needs-triage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d; body=%s", rr.Code, rr.Body.String())
	}

	// Update color + description.
	patch, _ := json.Marshal(map[string]any{"color": "00ff00", "description": "ready for triage"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/labels/needs-triage", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update: %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiLabel
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.Color != "00ff00" || updated.Description != "ready for triage" {
		t.Errorf("updated shape: %+v", updated)
	}

	// Delete.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/labels/needs-triage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d; body=%s", rr.Code, rr.Body.String())
	}

	// Re-get returns 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/labels/needs-triage", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get: %d", rr.Code)
	}
}

func TestLabels_CreateRejectsBadColor(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"name": "weird", "color": "not-hex"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestLabels_CreateRejectsDuplicate(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"name": "dup", "color": "112233"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first create: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("dup create: got %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestLabels_RequiresWriteScopeForCreate(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"name": "noscope", "color": "aabbcc"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestLabels_NonOwnerForbidden(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "bobs-label", "color": "445566"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
