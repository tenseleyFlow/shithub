// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	actionsdb "github.com/tenseleyFlow/shithub/internal/actions/sqlc"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

// seedCache inserts a workflow_caches row directly. Bypasses any
// future runner-side upload protocol so REST tests can exercise the
// list/delete surface without spinning up a runner fixture.
func seedCache(t *testing.T, pool *pgxpool.Pool, repoID int64, key, version, ref string) actionsdb.WorkflowCache {
	t.Helper()
	row, err := actionsdb.New().InsertWorkflowCache(context.Background(), pool, actionsdb.InsertWorkflowCacheParams{
		RepoID:       repoID,
		CacheKey:     key,
		CacheVersion: version,
		GitRef:       ref,
		ObjectKey:    "actions/caches/r" + strconv.FormatInt(repoID, 10) + "/" + key + "-" + version,
		SizeBytes:    1024,
	})
	if err != nil {
		t.Fatalf("InsertWorkflowCache: %v", err)
	}
	return row
}

type cachesListResponse struct {
	TotalCount    int64           `json:"total_count"`
	ActionsCaches []cacheRowEntry `json:"actions_caches"`
}

type cacheRowEntry struct {
	ID        int64  `json:"id"`
	Key       string `json:"key"`
	Version   string `json:"version"`
	Ref       string `json:"ref"`
	SizeBytes int64  `json:"size_bytes"`
}

func TestActionsCaches_ListReturnsSeededRows(t *testing.T) {
	pool, router, _, token, _, _ := seedBranchesEnv(t, "alice")
	repoID := getRepoIDForDemo(t, pool)
	seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/trunk")
	seedCache(t, pool, repoID, "go-mod", "v2", "refs/heads/feature/x")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/caches", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got cachesListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalCount != 2 {
		t.Errorf("total_count: got %d, want 2", got.TotalCount)
	}
	if len(got.ActionsCaches) != 2 {
		t.Fatalf("expected 2 caches; got %+v", got)
	}
}

func TestActionsCaches_FilterByKey(t *testing.T) {
	pool, router, _, token, _, _ := seedBranchesEnv(t, "alice")
	repoID := getRepoIDForDemo(t, pool)
	seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/trunk")
	seedCache(t, pool, repoID, "go-mod", "v2", "refs/heads/trunk")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/caches?key=node-modules", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got cachesListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.TotalCount != 1 || len(got.ActionsCaches) != 1 {
		t.Errorf("filter by key: %+v", got)
	}
	if got.ActionsCaches[0].Key != "node-modules" {
		t.Errorf("returned wrong row: %+v", got.ActionsCaches[0])
	}
}

func TestActionsCaches_FilterByRef(t *testing.T) {
	pool, router, _, token, _, _ := seedBranchesEnv(t, "alice")
	repoID := getRepoIDForDemo(t, pool)
	seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/trunk")
	seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/feature/x")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/caches?ref=refs/heads/trunk", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	var got cachesListResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &got)
	if got.TotalCount != 1 || len(got.ActionsCaches) != 1 {
		t.Errorf("filter by ref: %+v", got)
	}
	if got.ActionsCaches[0].Ref != "refs/heads/trunk" {
		t.Errorf("returned wrong row: %+v", got.ActionsCaches[0])
	}
}

func TestActionsCaches_DeleteByID(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	repoID := getRepoIDForDemo(t, pool)
	cache := seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/trunk")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/caches/"+strconv.FormatInt(cache.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	if _, err := actionsdb.New().GetWorkflowCacheByID(context.Background(), pool, cache.ID); err == nil {
		t.Errorf("row still present after delete")
	}
}

func TestActionsCaches_DeleteByIDUnknown404(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/caches/99999", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsCaches_DeleteByKey(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	repoID := getRepoIDForDemo(t, pool)
	seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/trunk")
	seedCache(t, pool, repoID, "node-modules", "v2", "refs/heads/trunk")
	seedCache(t, pool, repoID, "go-mod", "v1", "refs/heads/trunk")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/caches?key=node-modules", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	// Only go-mod should remain.
	count, _ := actionsdb.New().CountWorkflowCachesForRepo(context.Background(), pool, actionsdb.CountWorkflowCachesForRepoParams{RepoID: repoID})
	if count != 1 {
		t.Errorf("remaining caches: got %d, want 1 (go-mod)", count)
	}
}

func TestActionsCaches_DeleteByKeyRequiresKeyParam(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	writeToken := mintRunnerAPIPAT(t, pool, ownerIDForAlice(t, pool), string(pat.ScopeRepoWrite))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/caches", nil)
	req.Header.Set("Authorization", "Bearer "+writeToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsCaches_DeleteRequiresRepoWrite(t *testing.T) {
	_, router, _, token, _, _ := seedBranchesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/caches/1", nil)
	req.Header.Set("Authorization", "Bearer "+token) // repo:read only
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsCaches_CrossRepo404OnDeleteByID(t *testing.T) {
	pool, router, _, _, _, _ := seedBranchesEnv(t, "alice")
	repoID := getRepoIDForDemo(t, pool)
	cache := seedCache(t, pool, repoID, "node-modules", "v1", "refs/heads/trunk")

	// bob's repo + token, trying to delete alice's cache.
	bobID := seedRepoCreatorUser(t, pool, "bob")
	// We don't need to actually create bob's repo for this guard test;
	// the URL path `/repos/alice/demo/.../caches/<id>` still resolves
	// to alice's repo and the cross-repo check is on the run's repo_id.
	// We just need a bob token without alice's repo permissions.
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))
	_ = bobID

	// Sanity: deletion targets alice's URL but bob's PAT — policy should
	// resolve repo and gate via ActionRepoWrite; bob has no write on
	// alice/demo, so the deny lands at 404 (existence-leak-safe).
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/caches/"+strconv.FormatInt(cache.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s; url=%s", rr.Code, rr.Body.String(), req.URL.String())
	}
	if !strings.Contains(rr.Body.String(), "not found") {
		t.Errorf("body should be a not-found envelope: %s", rr.Body.String())
	}
}

// getRepoIDForDemo resolves alice/demo's repo_id. seedBranchesEnv
// doesn't return it directly; this helper covers the gap.
func getRepoIDForDemo(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		"SELECT id FROM repos WHERE name = 'demo' AND owner_user_id = (SELECT id FROM users WHERE username = 'alice')",
	).Scan(&id); err != nil {
		t.Fatalf("lookup demo repo: %v", err)
	}
	return id
}
