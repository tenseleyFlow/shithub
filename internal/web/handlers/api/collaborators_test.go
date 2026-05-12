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

type apiCollab struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

type apiPermission struct {
	User       string `json:"user"`
	Permission string `json:"permission"`
}

func putCollaborator(t *testing.T, router http.Handler, token, owner, repo, username, role string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"role": role})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/"+owner+"/"+repo+"/collaborators/"+username, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("put collaborator status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCollaborators_PutAndListAndRemove(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")
	seedRepoCreatorUser(t, pool, "bob")

	putCollaborator(t, router, token, "alice", "demo", "bob", "write")

	// list
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/collaborators", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiCollab
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 || listed[0].Username != "bob" || listed[0].Role != "write" {
		t.Errorf("shape: %+v", listed)
	}

	// membership probe (204)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/collaborators/bob", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("membership status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// permission level
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/collaborators/bob/permission", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("permission status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var perm apiPermission
	if err := json.Unmarshal(rr.Body.Bytes(), &perm); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if perm.Permission != "write" {
		t.Errorf("permission: got %q, want write", perm.Permission)
	}

	// remove
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/collaborators/bob", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCollaborators_MembershipUnknown404(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")
	seedRepoCreatorUser(t, pool, "bob")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/collaborators/bob", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCollaborators_PermissionForNonCollabReturnsNone(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")
	seedRepoCreatorUser(t, pool, "bob")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/collaborators/bob/permission", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var perm apiPermission
	_ = json.Unmarshal(rr.Body.Bytes(), &perm)
	if perm.Permission != "none" {
		t.Errorf("permission: got %q, want none", perm.Permission)
	}
}

func TestCollaborators_RejectsOwnerEnrollment(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"role": "admin"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/collaborators/alice", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestCollaborators_RejectsBadRole(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")
	seedRepoCreatorUser(t, pool, "bob")

	body, _ := json.Marshal(map[string]any{"role": "godmode"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/collaborators/bob", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	_ = pool
}

func TestCollaborators_NonOwnerCannotMutate(t *testing.T) {
	pool, router, _, _, _ := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"role": "write"})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/collaborators/bob", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		// Existence-leak guard: alice/demo is public so bob can see
		// it (200 on the GET). But for the PUT, ActionRepoAdmin gate
		// returns 404 to avoid leaking the existence-of-admin gap.
		// In current policy, public-repo non-collab probing for
		// ActionRepoAdmin yields 403; accept either 403 or 404 here
		// since both are "you can't do this".
		if rr.Code != http.StatusForbidden {
			t.Fatalf("status: got %d, want 403/404; body=%s", rr.Code, rr.Body.String())
		}
	}
}

func TestCollaborators_GhStyleRoleAliases(t *testing.T) {
	pool, router, _, _, token := seedIssuesEnv(t, "alice")
	seedRepoCreatorUser(t, pool, "bob")

	// "push" → write
	putCollaborator(t, router, token, "alice", "demo", "bob", "push")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/collaborators/bob/permission", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var perm apiPermission
	_ = json.Unmarshal(rr.Body.Bytes(), &perm)
	if perm.Permission != "write" {
		t.Errorf("gh-alias mapping: got %q, want write", perm.Permission)
	}
	_ = pool
}
