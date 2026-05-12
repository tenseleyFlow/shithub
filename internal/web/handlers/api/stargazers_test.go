// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/repos"
	socialdb "github.com/tenseleyFlow/shithub/internal/social/sqlc"
)

type apiStargazer struct {
	UserID      int64  `json:"user_id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
	StarredAt   string `json:"starred_at"`
}

type apiStarredRepo struct {
	RepoID     int64  `json:"repo_id"`
	OwnerSlug  string `json:"owner"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	StarCount  int64  `json:"star_count"`
	StarredAt  string `json:"starred_at"`
}

// seedStar inserts a (user_id, repo_id) row in `stars`. Bypasses the
// rate-limited social.Star orchestrator so tests don't need a fully
// wired throttle limiter or domain-event plumbing.
func seedStar(t *testing.T, pool *pgxpool.Pool, userID, repoID int64) {
	t.Helper()
	if err := socialdb.New().InsertStar(context.Background(), pool, socialdb.InsertStarParams{
		UserID: userID, RepoID: repoID,
	}); err != nil {
		t.Fatalf("seedStar: %v", err)
	}
}

func TestStargazers_RepoListReturnsBothStargazers(t *testing.T) {
	pool, router, aliceID, repoID, token := seedIssuesEnv(t, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	seedStar(t, pool, aliceID, repoID)
	seedStar(t, pool, bobID, repoID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/stargazers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiStargazer
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 stargazers; got %+v", listed)
	}
	// Recency-sorted: most recent insert is first. bob's star came after alice's.
	if listed[0].Username != "bob" {
		t.Errorf("recency-sort: first should be bob; got %+v", listed[0])
	}
	if listed[1].Username != "alice" {
		t.Errorf("recency-sort: second should be alice; got %+v", listed[1])
	}
}

func TestStargazers_RepoListRequiresRepoRead(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/stargazers", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestStargazers_RepoListUnknownRepo404(t *testing.T) {
	_, router, _, _, token := seedIssuesEnv(t, "alice")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/ghost/stargazers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestStargazers_UserStarredListsPublicRepos(t *testing.T) {
	pool, router, aliceID, demoID, _ := seedIssuesEnv(t, "alice")
	// Seed a second public repo so we can prove pagination/order work.
	rfs, rerr := storage.NewRepoFS(t.TempDir())
	if rerr != nil {
		t.Fatalf("NewRepoFS: %v", rerr)
	}
	res, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   aliceID,
		OwnerUserID:   aliceID,
		OwnerUsername: "alice",
		Name:          "second",
		Description:   "second public",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("create second repo: %v", err)
	}
	seedStar(t, pool, aliceID, demoID)
	seedStar(t, pool, aliceID, res.Repo.ID)

	aliceToken := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/starred", nil)
	req.Header.Set("Authorization", "Bearer "+aliceToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiStarredRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 starred repos; got %+v", listed)
	}
	if listed[0].Name != "second" {
		t.Errorf("recency-sort: first should be second; got %+v", listed[0])
	}
	for _, repo := range listed {
		if repo.OwnerSlug != "alice" {
			t.Errorf("owner slug: want alice; got %+v", repo)
		}
		if repo.Visibility != "public" {
			t.Errorf("visibility: want public; got %+v", repo)
		}
	}
}

func TestStargazers_UserStarredHidesPrivateFromOtherViewer(t *testing.T) {
	pool, router, aliceID, demoID, _ := seedIssuesEnv(t, "alice")
	rfs, rerr := storage.NewRepoFS(t.TempDir())
	if rerr != nil {
		t.Fatalf("NewRepoFS: %v", rerr)
	}
	priv, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   aliceID,
		OwnerUserID:   aliceID,
		OwnerUsername: "alice",
		Name:          "secret",
		Description:   "private",
		Visibility:    "private",
	})
	if err != nil {
		t.Fatalf("create private repo: %v", err)
	}
	seedStar(t, pool, aliceID, demoID)
	seedStar(t, pool, aliceID, priv.Repo.ID)

	bobID := seedRepoCreatorUser(t, pool, "bob")
	bobToken := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/starred", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiStarredRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, repo := range listed {
		if repo.Name == "secret" {
			t.Errorf("private repo leaked into cross-user starred list: %+v", repo)
		}
		if repo.Visibility != "public" {
			t.Errorf("non-public visibility leaked: %+v", repo)
		}
	}
	// Must still include the public demo repo.
	var foundDemo bool
	for _, repo := range listed {
		if repo.Name == "demo" {
			foundDemo = true
		}
	}
	if !foundDemo {
		t.Errorf("public demo missing from cross-user view; got %+v", listed)
	}
}

func TestStargazers_UserStarredUnknownUser404(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	token := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/ghost/starred", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestStargazers_UserStarredRequiresUserRead(t *testing.T) {
	pool, router, aliceID, _, _ := seedIssuesEnv(t, "alice")
	wrong := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/starred", nil)
	req.Header.Set("Authorization", "Bearer "+wrong)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
