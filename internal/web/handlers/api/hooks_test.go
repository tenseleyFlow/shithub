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
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/repos"
	"github.com/tenseleyFlow/shithub/internal/security/ssrf"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

type apiHook struct {
	ID                  int64    `json:"id"`
	OwnerKind           string   `json:"owner_kind"`
	OwnerID             int64    `json:"owner_id"`
	URL                 string   `json:"url"`
	ContentType         string   `json:"content_type"`
	Events              []string `json:"events"`
	Active              bool     `json:"active"`
	SSLVerification     bool     `json:"ssl_verification"`
	ConsecutiveFailures int32    `json:"consecutive_failures"`
	CreatedAt           string   `json:"created_at"`
	UpdatedAt           string   `json:"updated_at"`
}

type apiDelivery struct {
	ID           int64  `json:"id"`
	HookID       int64  `json:"hook_id"`
	EventKind    string `json:"event_kind"`
	Status       string `json:"status"`
	Attempt      int32  `json:"attempt"`
	MaxAttempts  int32  `json:"max_attempts"`
	DeliveryUUID string `json:"delivery_uuid"`
}

// newHooksAPIRouter builds an API router with the RepoFS + SecretBox
// + permissive WebhookSSRF (AllowPrivateNetworks) so test hook URLs
// targeting loopback hosts validate without going near the real
// network. Mirrors newReposAPIRouter with the webhook-specific knobs.
func newHooksAPIRouter(t *testing.T, pool *pgxpool.Pool) (http.Handler, *storage.RepoFS) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewRepoFS: %v", err)
	}
	box := testRunnerAPISecretBox(t)
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RepoFS:      rfs,
		Audit:       audit.NewRecorder(),
		Throttle:    throttle.NewLimiter(),
		RateLimiter: ratelimit.New(pool),
		SecretBox:   box,
		BaseURL:     "https://shithub.test",
		APILimit: apilimit.Config{
			AuthedPerHour: 5000,
			AnonPerHour:   60,
			Logger:        logger,
		},
		WebhookSSRF: ssrf.Config{AllowPrivateNetworks: true},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r, rfs
}

// seedHooksRepo creates a public repo for the supplied user via the
// repos.Create orchestrator (skipping ShithubdPath so the post-receive
// hook install is a no-op).
func seedHooksRepo(t *testing.T, pool *pgxpool.Pool, rfs *storage.RepoFS, userID int64, ownerUsername, name string) int64 {
	t.Helper()
	res, err := repos.Create(context.Background(), repos.Deps{
		Pool:    pool,
		RepoFS:  rfs,
		Audit:   audit.NewRecorder(),
		Limiter: throttle.NewLimiter(),
	}, repos.Params{
		ActorUserID:   userID,
		OwnerUserID:   userID,
		OwnerUsername: ownerUsername,
		Name:          name,
		Description:   name + " repo",
		Visibility:    "public",
	})
	if err != nil {
		t.Fatalf("repos.Create %q: %v", name, err)
	}
	return res.Repo.ID
}

func createHook(t *testing.T, router http.Handler, token, owner, repo, hookURL string, events []string) apiHook {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"url":          hookURL,
		"content_type": "json",
		"events":       events,
		"active":       true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/"+owner+"/"+repo+"/hooks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create hook status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var out apiHook
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode hook: %v; body=%s", err, rr.Body.String())
	}
	return out
}

func TestHooks_CreateAndGet(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")

	created := createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/sink", []string{"push", "issues"})
	if created.URL != "https://127.0.0.1:443/sink" {
		t.Errorf("url: got %q", created.URL)
	}
	if !created.Active || !created.SSLVerification {
		t.Errorf("defaults: active=%v ssl=%v", created.Active, created.SSLVerification)
	}
	if len(created.Events) != 2 {
		t.Errorf("events: %+v", created.Events)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/hooks/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got apiHook
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("round-trip id: got %d, want %d", got.ID, created.ID)
	}
}

func TestHooks_List(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")
	createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/a", []string{"push"})
	createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/b", []string{"issues"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/hooks", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiHook
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) != 2 {
		t.Errorf("len: got %d, want 2; payload=%+v", len(listed), listed)
	}
}

func TestHooks_Patch(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")
	created := createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/sink", []string{"push"})

	body, _ := json.Marshal(map[string]any{
		"url":    "https://127.0.0.1:443/changed",
		"events": []string{"push", "pull_request"},
		"active": false,
	})
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/repos/alice/demo/hooks/"+strconv.FormatInt(created.ID, 10), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var updated apiHook
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if updated.URL != "https://127.0.0.1:443/changed" {
		t.Errorf("url not updated: %s", updated.URL)
	}
	if updated.Active {
		t.Errorf("active not flipped off")
	}
	if len(updated.Events) != 2 {
		t.Errorf("events: %+v", updated.Events)
	}
}

func TestHooks_Delete(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")
	created := createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/sink", []string{"push"})

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/hooks/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/hooks/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHooks_RequiresWriteScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")
	readOnly := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/hooks", nil)
	req.Header.Set("Authorization", "Bearer "+readOnly)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHooks_CrossRepoReturns404(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")
	seedHooksRepo(t, pool, rfs, userID, "alice", "other")
	created := createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/sink", []string{"push"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/other/hooks/"+strconv.FormatInt(created.ID, 10), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHooks_RejectsBadURL(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")

	body, _ := json.Marshal(map[string]any{
		"url":          "ftp://example.com/payload",
		"content_type": "json",
		"events":       []string{"push"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/repos/alice/demo/hooks", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHooks_DeliveriesListIncludesPing(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router, rfs := newHooksAPIRouter(t, pool)
	userID := seedRepoCreatorUser(t, pool, "alice")
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite))
	seedHooksRepo(t, pool, rfs, userID, "alice", "demo")
	created := createHook(t, router, token, "alice", "demo", "https://127.0.0.1:443/sink", []string{"push"})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/hooks/"+strconv.FormatInt(created.ID, 10)+"/deliveries", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiDelivery
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(listed) < 1 {
		t.Fatalf("len: got %d, want ≥1 (ping); payload=%+v", len(listed), listed)
	}
	if listed[0].HookID != created.ID {
		t.Errorf("delivery hook_id: got %d, want %d", listed[0].HookID, created.ID)
	}
	if listed[0].EventKind == "" {
		t.Errorf("event_kind empty: %+v", listed[0])
	}
}
