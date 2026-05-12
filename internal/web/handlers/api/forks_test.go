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

type apiFork struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	OwnerLogin    string `json:"owner_login"`
	OwnerName     string `json:"owner_display_name"`
	Description   string `json:"description"`
	Visibility    string `json:"visibility"`
	InitStatus    string `json:"init_status"`
	CreatedAt     string `json:"created_at"`
	SourceRepoID  int64  `json:"source_repo_id"`
	DefaultBranch string `json:"default_branch"`
}

func TestForks_CreateInSelfNamespace(t *testing.T) {
	// alice owns alice/demo (public). bob forks it into his own
	// account with the default name → result row carries
	// init_pending init_status + bob as owner_login.
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/forks", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiFork
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "demo" {
		t.Errorf("name: %q", got.Name)
	}
	if got.OwnerLogin != "bob" {
		t.Errorf("owner_login: %q", got.OwnerLogin)
	}
	if got.InitStatus == "" {
		t.Errorf("init_status empty: %+v", got)
	}
	if got.SourceRepoID == 0 {
		t.Errorf("source_repo_id missing: %+v", got)
	}
}

func TestForks_CreateWithCustomName(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo-fork"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/forks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiFork
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.Name != "demo-fork" {
		t.Errorf("custom name: %q", got.Name)
	}
}

func TestForks_SelfForkSameNameRejected(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	// alice can't fork alice/demo into alice/demo (same name).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/forks", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestForks_List(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	// bob forks alice/demo.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/forks", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed fork: %d; body=%s", rr.Code, rr.Body.String())
	}

	// Anyone with repo:read on alice/demo lists the fork.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/forks", nil)
	listReq.Header.Set("Authorization", "Bearer "+bobToken)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list status: got %d; body=%s", listRR.Code, listRR.Body.String())
	}
	var listed []apiFork
	if err := json.Unmarshal(listRR.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("len: got %d, want 1; payload=%+v", len(listed), listed)
	}
	if listed[0].OwnerLogin != "bob" {
		t.Errorf("owner: %+v", listed[0])
	}
}

func TestForks_RequiresRepoWriteScopeOnPOST(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	readOnly := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/forks", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestForks_UnknownSource404(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/ghost/forks", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
