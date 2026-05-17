// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

// apiRepoOwner mirrors repoOwnerEnvelope.
type apiRepoOwner struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// apiRepoLicense mirrors repoLicenseEnvelope.
type apiRepoLicense struct {
	Key string `json:"key"`
}

type apiRepo struct {
	ID            int64           `json:"id"`
	Name          string          `json:"name"`
	FullName      string          `json:"full_name"`
	OwnerLogin    string          `json:"owner_login"`
	OwnerType     string          `json:"owner_type"`
	Owner         *apiRepoOwner   `json:"owner"`
	Description   string          `json:"description"`
	Visibility    string          `json:"visibility"`
	Private       bool            `json:"private"`
	HTMLURL       string          `json:"html_url"`
	DefaultBranch string          `json:"default_branch"`
	Fork          bool            `json:"fork"`
	Archived      bool            `json:"archived"`
	IsTemplate    bool            `json:"is_template"`
	HasIssues     bool            `json:"has_issues"`
	HasPulls      bool            `json:"has_pulls"`
	StarCount     int64           `json:"star_count"`
	WatcherCount  int64           `json:"watcher_count"`
	ForkCount     int64           `json:"fork_count"`
	Topics        []string        `json:"topics"`
	License       *apiRepoLicense `json:"license"`
	Language      string          `json:"language"`
	Size          int64           `json:"size"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	PushedAt      string          `json:"pushed_at"`
}

// newReposAPIRouter builds an API router with the repo-create stack
// wired in: Audit, Throttle, and a per-test RepoFS rooted at t.TempDir.
// ShithubdPath is left empty so hook installation is a no-op (matches
// the repos.Create test fixtures).
func newReposAPIRouter(t *testing.T, pool *pgxpool.Pool) (http.Handler, *storage.RepoFS) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewRepoFS: %v", err)
	}
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RepoFS:      rfs,
		Audit:       audit.NewRecorder(),
		Throttle:    throttle.NewLimiter(),
		RateLimiter: ratelimit.New(pool),
		BaseURL:     "https://shithub.test",
		APILimit: apilimit.Config{
			AuthedPerHour: 5000,
			AnonPerHour:   60,
			Logger:        logger,
		},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r, rfs
}

func seedRepoCreatorUser(t *testing.T, pool *pgxpool.Pool, username string) (userID int64) {
	t.Helper()
	ctx := context.Background()
	q := usersdb.New()
	user, err := q.CreateUser(ctx, pool, usersdb.CreateUserParams{
		Username:     username,
		DisplayName:  strings.ToUpper(username[:1]) + username[1:],
		PasswordHash: runnerAPIFixtureHash,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	em, err := q.CreateUserEmail(ctx, pool, usersdb.CreateUserEmailParams{
		UserID:    user.ID,
		Email:     username + "@example.test",
		IsPrimary: true,
	})
	if err != nil {
		t.Fatalf("CreateUserEmail: %v", err)
	}
	if err := q.MarkUserEmailVerified(ctx, pool, em.ID); err != nil {
		t.Fatalf("MarkUserEmailVerified: %v", err)
	}
	if err := q.LinkUserPrimaryEmail(ctx, pool, usersdb.LinkUserPrimaryEmailParams{
		ID:             user.ID,
		PrimaryEmailID: pgtype.Int8{Int64: em.ID, Valid: true},
	}); err != nil {
		t.Fatalf("LinkUserPrimaryEmail: %v", err)
	}
	if err := q.MarkUserEmailPrimaryVerified(ctx, pool, user.ID); err != nil {
		t.Fatalf("MarkUserEmailPrimaryVerified: %v", err)
	}
	return user.ID
}

func TestRepos_CreatePersonalAndGet(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{
		"name":        "demo",
		"description": "first cut",
		"visibility":  "public",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v; body=%s", err, rr.Body.String())
	}
	if created.Name != "demo" || created.OwnerLogin != "alice" || created.OwnerType != "user" {
		t.Errorf("create payload: %+v", created)
	}
	if created.Visibility != "public" || created.Private {
		t.Errorf("visibility: got %+v, want public/public", created)
	}
	if created.DefaultBranch != "trunk" {
		t.Errorf("default_branch: got %q, want trunk", created.DefaultBranch)
	}
	// S62 audit B14: nested owner envelope populated alongside legacy
	// flat fields. CLI's `repo view --json owner` rendered {login:"",
	// type:""} before this; pin the envelope so a future regression
	// surfaces in CI instead of in a user's terminal.
	if created.Owner == nil || created.Owner.Login != "alice" || created.Owner.Type != "User" {
		t.Errorf("owner envelope: %+v", created.Owner)
	}
	// HTMLURL is BaseURL + "/" + full_name. The test router uses
	// "https://shithub.test" so we can assert the prefix.
	if !strings.HasPrefix(created.HTMLURL, "https://shithub.test/alice/demo") {
		t.Errorf("html_url: got %q, want https://shithub.test/alice/demo*", created.HTMLURL)
	}
	// pushed_at is best-effort (= updated_at); just confirm it is set.
	if created.PushedAt == "" {
		t.Error("pushed_at should be populated")
	}

	// GET single repo.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var fetched apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode get: %v", err)
	}
	if fetched.Owner == nil || fetched.Owner.Login != "alice" {
		t.Errorf("fetched owner envelope: %+v", fetched.Owner)
	}
	if fetched.HTMLURL == "" {
		t.Error("fetched html_url should be populated")
	}
}

func TestRepos_CreateRejectsBadName(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "BAD..NAME", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_CreateRejectsDuplicate(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	mk := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		return rr
	}
	if rr := mk(); rr.Code != http.StatusCreated {
		t.Fatalf("first create: %d", rr.Code)
	}
	rr := mk()
	if rr.Code != http.StatusConflict {
		t.Fatalf("dup create: got %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_CreateRequiresRepoWriteScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_ListAuthedUserSeesPrivate(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	for _, spec := range []struct {
		name, vis string
	}{
		{"demo-public", "public"},
		{"demo-private", "private"},
	} {
		body, _ := json.Marshal(map[string]any{"name": spec.name, "visibility": spec.vis})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d; body=%s", spec.name, rr.Code, rr.Body.String())
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/repos", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200", rr.Code)
	}
	var listed []apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("count: got %d, want 2; %+v", len(listed), listed)
	}
}

func TestRepos_ListOtherUserPublicOnly(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))

	// Alice creates one public + one private.
	for _, spec := range []struct{ name, vis string }{
		{"public-one", "public"},
		{"private-one", "private"},
	} {
		body, _ := json.Marshal(map[string]any{"name": spec.name, "visibility": spec.vis})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tokenAlice)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed %s: %d", spec.name, rr.Code)
		}
	}

	// Bob lists alice's repos — should see only the public one.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/alice/repos", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rr.Code)
	}
	var listed []apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "public-one" {
		t.Fatalf("public-only filter failed: %+v", listed)
	}
}

func TestRepos_GetPrivateHidesFromOthers(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoRead))

	body, _ := json.Marshal(map[string]any{"name": "secret", "visibility": "private"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d; %s", rr.Code, rr.Body.String())
	}

	// Bob asks for the private repo directly — must 404 (existence leak).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/secret", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_PatchDescriptionAndVisibility(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public", "description": "old"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"description": "new", "visibility": "private"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiRepo
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if updated.Description != "new" {
		t.Errorf("description: got %q, want %q", updated.Description, "new")
	}
	if updated.Visibility != "private" || !updated.Private {
		t.Errorf("visibility: got %+v", updated)
	}
}

func TestRepos_PatchRejectsNonOwner(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	patch, _ := json.Marshal(map[string]any{"description": "evil"})
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo", bytes.NewReader(patch))
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_DeleteSoftDeletes(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "throwaway", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/throwaway", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// Subsequent GET 404s.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/throwaway", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRepos_DeleteOnlyOwners(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, _ := newReposAPIRouter(t, pool)
	aliceID := seedRepoCreatorUser(t, pool, "alice")
	bobID := seedRepoCreatorUser(t, pool, "bob")
	tokenAlice := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeRepoWrite))
	tokenBob := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeRepoWrite))

	body, _ := json.Marshal(map[string]any{"name": "demo", "visibility": "public"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/repos", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenAlice)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed: %d", rr.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo", nil)
	req.Header.Set("Authorization", "Bearer "+tokenBob)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}
