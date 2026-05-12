// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	issuesdb "github.com/tenseleyFlow/shithub/internal/issues/sqlc"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

type apiIssue struct {
	ID          int64    `json:"id"`
	Number      int64    `json:"number"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason"`
	Locked      bool     `json:"locked"`
	LockReason  string   `json:"lock_reason"`
	AuthorID    int64    `json:"author_id"`
	Labels      []string `json:"labels"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
	ClosedAt    string   `json:"closed_at"`
}

type apiComment struct {
	ID        int64  `json:"id"`
	IssueID   int64  `json:"issue_id"`
	AuthorID  int64  `json:"author_id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// seedIssuesEnv stands up a one-shot test environment: pool + router +
// owner user + a public repo (`alice/demo` by default) + a PAT scoped
// to repo:write. Tests that need a second actor (Bob) build him with
// seedRepoCreatorUser against the returned pool.
func seedIssuesEnv(t *testing.T, ownerUsername string) (pool *pgxpool.Pool, router http.Handler, userID, repoID int64, token string) {
	t.Helper()
	pool = dbtest.NewTestDB(t)
	router, rfs := newReposAPIRouter(t, pool)
	userID = seedRepoCreatorUser(t, pool, ownerUsername)
	token = mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	res, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   userID,
		OwnerUserID:   userID,
		OwnerUsername: ownerUsername,
		Name:          "demo",
		Description:   "demo repo",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("repos.Create: %v", err)
	}
	return pool, router, userID, res.Repo.ID, token
}

func TestIssues_CreateAndGet(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "first bug", "body": "kaboom"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Number != 1 || created.Title != "first bug" || created.State != "open" {
		t.Errorf("shape: %+v", created)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CreateRejectsEmptyTitle(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "", "body": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CreateRequiresRepoWriteScope(t *testing.T) {
	pool, router, userID, _, _ := seedIssuesEnv(t, "alice")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"title": "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_ListFiltersByState(t *testing.T) {
	pool, router, userID, repoID, token := seedIssuesEnv(t, "alice")
	// Create two issues, close the second directly via sqlc.
	for i, title := range []string{"open one", "closed one"} {
		body, _ := json.Marshal(map[string]any{"title": title})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %d: %d", i, rr.Code)
		}
	}
	q := issuesdb.New()
	issue, err := q.GetIssueByNumber(context.Background(), pool, issuesdb.GetIssueByNumberParams{
		RepoID: repoID, Number: 2,
	})
	if err != nil {
		t.Fatalf("GetIssueByNumber: %v", err)
	}
	if err := q.SetIssueState(context.Background(), pool, issuesdb.SetIssueStateParams{
		ID:             issue.ID,
		State:          issuesdb.IssueStateClosed,
		StateReason:    issuesdb.NullIssueStateReason{Valid: false},
		ClosedByUserID: pgtype.Int8{Int64: userID, Valid: true},
	}); err != nil {
		t.Fatalf("SetIssueState: %v", err)
	}

	for _, tc := range []struct {
		state string
		want  int
	}{
		{"open", 1},
		{"closed", 1},
		{"all", 2},
		{"", 2},
	} {
		url := "/api/v1/repos/alice/demo/issues"
		if tc.state != "" {
			url += "?state=" + tc.state
		}
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("state=%q: %d; body=%s", tc.state, rr.Code, rr.Body.String())
		}
		var listed []apiIssue
		if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(listed) != tc.want {
			t.Errorf("state=%q count: got %d, want %d", tc.state, len(listed), tc.want)
		}
	}
}

func TestIssues_PatchTitleBodyState(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "old", "body": "old body"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"title": "new", "body": "new body", "state": "closed", "state_reason": "completed"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiIssue
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.Title != "new" || updated.Body != "new body" {
		t.Errorf("title/body: %+v", updated)
	}
	if updated.State != "closed" || updated.StateReason != "completed" {
		t.Errorf("state: %+v", updated)
	}
}

func TestIssues_PatchTitleByOtherForbidden(t *testing.T) {
	pool, router, _, _, tokenAlice := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "alice's bug"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	patch, _ := json.Marshal(map[string]any{"title": "hijack"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/issues/1", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CommentsCRUD(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")
	body, _ := json.Marshal(map[string]any{"title": "needs feedback"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue seed: %d", rr.Code)
	}

	body, _ = json.Marshal(map[string]any{"body": "lgtm"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment create: %d; body=%s", rr.Code, rr.Body.String())
	}
	var created apiComment
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Body != "lgtm" || created.IssueID == 0 {
		t.Errorf("shape: %+v", created)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1/comments", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: %d", rr.Code)
	}
	var listed []apiComment
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("list: %+v", listed)
	}

	patch, _ := json.Marshal(map[string]any{"body": "lgtm — second look"})
	req = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/repos/alice/demo/issues/comments/%d", created.ID), bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("update status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/v1/repos/alice/demo/issues/comments/%d", created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_CommentEditByNonAuthorForbidden(t *testing.T) {
	pool, router, _, _, tokenAlice := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "thread"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue seed: %d", rr.Code)
	}

	body, _ = json.Marshal(map[string]any{"body": "alice's comment"})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues/1/comments", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("comment seed: %d", rr.Code)
	}
	var created apiComment
	_ = json.Unmarshal(rr.Body.Bytes(), &created)

	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	patch, _ := json.Marshal(map[string]any{"body": "hijacked"})
	req = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/repos/alice/demo/issues/comments/%d", created.ID), bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestIssues_LockUnlock(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	body, _ := json.Marshal(map[string]any{"title": "spicy"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/issues", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/issues/1/lock", strings.NewReader(`{"lock_reason":"off-topic"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("lock status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/issues/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d", rr.Code)
	}
	var fetched apiIssue
	_ = json.Unmarshal(rr.Body.Bytes(), &fetched)
	if !fetched.Locked || fetched.LockReason != "off-topic" {
		t.Errorf("locked state: %+v", fetched)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/issues/1/lock", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("unlock status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}
