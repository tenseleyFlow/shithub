// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

type apiMilestone struct {
	ID           int64  `json:"id"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	DueOn        string `json:"due_on"`
	OpenIssues   int32  `json:"open_issues"`
	ClosedIssues int32  `json:"closed_issues"`
	CreatedAt    string `json:"created_at"`
}

type apiAssignee struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

func createMilestone(t *testing.T, router http.Handler, token, owner, repo, title string) apiMilestone {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"title": title, "description": "for testing"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+owner+"/"+repo+"/milestones", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create milestone status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var m apiMilestone
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	return m
}

func TestMilestones_CreateAndList(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	createMilestone(t, router, token, "alice", "demo", "v1")
	createMilestone(t, router, token, "alice", "demo", "v2")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/milestones", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiMilestone
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("len: got %d, want 2", len(listed))
	}
}

func TestMilestones_StateFilter(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	opened := createMilestone(t, router, token, "alice", "demo", "open-one")
	closeable := createMilestone(t, router, token, "alice", "demo", "close-me")

	// Close one milestone via PATCH.
	body, _ := json.Marshal(map[string]any{"state": "closed"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/milestones/"+strconv.FormatInt(closeable.ID, 10), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("close patch status: got %d; body=%s", rr.Code, rr.Body.String())
	}

	for _, tc := range []struct {
		state string
		want  int64
	}{
		{"open", opened.ID},
		{"closed", closeable.ID},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/milestones?state="+tc.state, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("filter %q status: got %d; body=%s", tc.state, rr.Code, rr.Body.String())
		}
		var listed []apiMilestone
		if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(listed) != 1 || listed[0].ID != tc.want {
			t.Errorf("filter %q: got %+v", tc.state, listed)
		}
	}
}

func TestMilestones_PatchUpdatesTitle(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	m := createMilestone(t, router, token, "alice", "demo", "before")

	body, _ := json.Marshal(map[string]any{"title": "after", "description": "updated"})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/milestones/"+strconv.FormatInt(m.ID, 10), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiMilestone
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Title != "after" || updated.Description != "updated" {
		t.Errorf("shape: %+v", updated)
	}
}

func TestMilestones_Delete(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	m := createMilestone(t, router, token, "alice", "demo", "gone")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/milestones/"+strconv.FormatInt(m.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	// Follow-up GET → 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/milestones/"+strconv.FormatInt(m.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get status: got %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestMilestones_CrossRepoReturns404(t *testing.T) {
	pool, router, userID, _, tokenAlice := seedIssuesEnv(t, "alice")
	m := createMilestone(t, router, tokenAlice, "alice", "demo", "for-alice")

	// Bob has his own repo `bob/playground`; probing alice's milestone
	// id under that path must 404 even though the id exists globally.
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))
	_ = userID
	// Materialise bob's repo via the API to skip touching repos.Create
	// internals.
	body, _ := json.Marshal(map[string]any{"name": "playground", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed bob repo: got %d; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/bob/playground/milestones/"+strconv.FormatInt(m.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-repo get: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestAssignees_ListIncludesOwner(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/assignees", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiAssignee
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, a := range listed {
		if a.Username == "alice" && a.Role == "owner" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected alice as owner in: %+v", listed)
	}
}

func TestIssues_PatchAttachLabelAndMilestone(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	// Issue + label + milestone seed.
	createIssueBody, _ := json.Marshal(map[string]any{"title": "needs triage"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(createIssueBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create issue: %d; body=%s", rr.Code, rr.Body.String())
	}
	var issue apiIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &issue)

	// Use a non-default label name; repos.Create seeds the GitHub
	// classic set (bug, enhancement, …) so picking one of those would
	// 409 here.
	createLabelBody, _ := json.Marshal(map[string]any{"name": "needs-triage", "color": "ff0000"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/labels", bytes.NewReader(createLabelBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create label: %d; body=%s", rr.Code, rr.Body.String())
	}

	m := createMilestone(t, router, token, "alice", "demo", "v1.0")

	patchBody, _ := json.Marshal(map[string]any{
		"labels":    []string{"needs-triage"},
		"milestone": m.ID,
		"assignees": []string{"alice"},
	})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(issue.Number, 10), bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(updated.Labels) != 1 || updated.Labels[0].Name != "needs-triage" {
		t.Errorf("labels: %+v", updated.Labels)
	}
}

func TestIssues_PatchRejectsUnknownLabel(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	createIssueBody, _ := json.Marshal(map[string]any{"title": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(createIssueBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var issue apiIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &issue)

	patchBody, _ := json.Marshal(map[string]any{"labels": []string{"ghost"}})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(issue.Number, 10), bytes.NewReader(patchBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_PatchClearsMilestone(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	createIssueBody, _ := json.Marshal(map[string]any{"title": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(createIssueBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var issue apiIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &issue)

	m := createMilestone(t, router, token, "alice", "demo", "stub")
	// attach
	patch, _ := json.Marshal(map[string]any{"milestone": m.ID})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(issue.Number, 10), bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("attach: %d; body=%s", rr.Code, rr.Body.String())
	}
	// clear
	clear, _ := json.Marshal(map[string]any{"milestone": 0})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/"+strconv.FormatInt(issue.Number, 10), bytes.NewReader(clear))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("clear: %d; body=%s", rr.Code, rr.Body.String())
	}
}
