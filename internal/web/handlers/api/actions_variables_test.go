// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestActionsVariables_CreateListGetUpdateDelete(t *testing.T) {
	env := newSecretsTestEnv(t)

	// CREATE
	createBody, _ := json.Marshal(map[string]string{"name": "API_URL", "value": "https://api.example"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/variables", bytes.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body=%s", rr.Code, rr.Body.String())
	}

	// LIST
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/variables", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0]["name"] != "API_URL" || listed[0]["value"] != "https://api.example" {
		t.Errorf("list shape: %+v", listed)
	}

	// GET single
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/variables/API_URL", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get single: got %d; body=%s", rr.Code, rr.Body.String())
	}

	// PATCH (update value)
	updBody, _ := json.Marshal(map[string]string{"value": "https://api.example/v2"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/actions/variables/API_URL", bytes.NewReader(updBody))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &updated)
	if updated["value"] != "https://api.example/v2" {
		t.Errorf("patched value: %+v", updated)
	}

	// DELETE
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/variables/API_URL", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	rr = httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsVariables_CreateRejectsBadName(t *testing.T) {
	env := newSecretsTestEnv(t)
	body, _ := json.Marshal(map[string]string{"name": "1bad-name", "value": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/variables", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsVariables_GetUnknown404(t *testing.T) {
	env := newSecretsTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/variables/MISSING", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsVariables_CreateRequiresRepoWrite(t *testing.T) {
	env := newSecretsTestEnv(t)
	body, _ := json.Marshal(map[string]string{"name": "X", "value": "y"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/actions/variables", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
