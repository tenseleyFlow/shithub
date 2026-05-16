// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

// PRO-EXT01-13c: integration tests for /api/v1/user/cron-dispatches/*.

func TestUserCronDispatchesCRUD_CreateListDelete(t *testing.T) {
	pool, _, _, _, _, _ := seedBranchesEnv(t, "alice")
	router, _ := newReposAPIRouter(t, pool)
	userID := ownerIDForAlice(t, pool)
	upgradeUserToActivePro(t, pool, userID)
	repoID := repoIDForAliceDemo(t, pool)
	token := mintUserScopePAT(t, pool, userID)

	// CREATE.
	createBody, _ := json.Marshal(map[string]any{
		"repo_id":       repoID,
		"workflow_file": ".shithub/workflows/ci.yml",
		"ref":           "trunk",
		"cron_expr":     "0 * * * *",
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/user/cron-dispatches", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	router.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201; body=%s", createRR.Code, createRR.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(createRR.Body.Bytes(), &created)
	if created["cron_expr"] != "0 * * * *" {
		t.Errorf("cron_expr: got %v", created["cron_expr"])
	}
	if created["ref"] != "refs/heads/trunk" {
		t.Errorf("ref normalization: got %v, want refs/heads/trunk", created["ref"])
	}
	id := int64(created["id"].(float64))

	// LIST.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/cron-dispatches", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRR := httptest.NewRecorder()
	router.ServeHTTP(listRR, listReq)
	var listed []map[string]any
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Errorf("list: got %d rows, want 1", len(listed))
	}

	// DELETE.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/user/cron-dispatches/"+jsonInt64(id), nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delRR := httptest.NewRecorder()
	router.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", delRR.Code)
	}
}

func TestUserCronDispatchesCRUD_InvalidCronExprReturns400(t *testing.T) {
	pool, _, _, _, _, _ := seedBranchesEnv(t, "alice")
	router, _ := newReposAPIRouter(t, pool)
	userID := ownerIDForAlice(t, pool)
	upgradeUserToActivePro(t, pool, userID)
	repoID := repoIDForAliceDemo(t, pool)
	token := mintUserScopePAT(t, pool, userID)

	body, _ := json.Marshal(map[string]any{
		"repo_id":       repoID,
		"workflow_file": ".shithub/workflows/ci.yml",
		"ref":           "trunk",
		"cron_expr":     "garbage",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/cron-dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("garbage cron: got %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserCronDispatchesCRUD_NonOwnedRepoReturns404(t *testing.T) {
	pool, _, _, _, _, _ := seedBranchesEnv(t, "alice")
	router, _ := newReposAPIRouter(t, pool)
	upgradeUserToActivePro(t, pool, ownerIDForAlice(t, pool))
	repoID := repoIDForAliceDemo(t, pool)

	// Bob tries to schedule against Alice's repo.
	bobID := seedRepoCreatorUser(t, pool, "bob")
	upgradeUserToActivePro(t, pool, bobID)
	bobToken := mintUserScopePAT(t, pool, bobID)

	body, _ := json.Marshal(map[string]any{
		"repo_id":       repoID,
		"workflow_file": ".shithub/workflows/ci.yml",
		"ref":           "trunk",
		"cron_expr":     "0 * * * *",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/cron-dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bobToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-owner schedule: got %d, want 404", rr.Code)
	}
}

func TestUserCronDispatchesCRUD_FreeUserEnforceCreateReturns403(t *testing.T) {
	pool, _, _, _, _, _ := seedBranchesEnv(t, "alice")
	router := newReposAPIRouterWithEnforce(t, pool, true)
	userID := ownerIDForAlice(t, pool)
	repoID := repoIDForAliceDemo(t, pool)
	token := mintUserScopePAT(t, pool, userID)

	body, _ := json.Marshal(map[string]any{
		"repo_id":       repoID,
		"workflow_file": ".shithub/workflows/ci.yml",
		"ref":           "trunk",
		"cron_expr":     "0 * * * *",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/cron-dispatches", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("Free + enforce: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// newReposAPIRouterWithEnforce is newReposAPIRouter + a custom
// BillingEnforce config. SecretBox is wired too so any path that
// touches webhook-relay storage still works under enforce-on.
func newReposAPIRouterWithEnforce(t *testing.T, pool *pgxpool.Pool, enforceCron bool) http.Handler {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("storage.NewRepoFS: %v", err)
	}
	storageKey, _ := secretbox.GenerateKey()
	sBox, _ := secretbox.FromBytes(storageKey)
	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RepoFS:      rfs,
		SecretBox:   sBox,
		Audit:       audit.NewRecorder(),
		Throttle:    throttle.NewLimiter(),
		RateLimiter: ratelimit.New(pool),
		BaseURL:     "https://shithub.test",
		APILimit: apilimit.Config{
			AuthedPerHour: 5000, AnonPerHour: 60, Logger: logger,
		},
		BillingEnforce: config.EnforceConfig{UserCronWorkflowDispatch: enforceCron},
	})
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	return r
}

// Silence unused-import warnings if a future trim drops pat from any test.
var _ = pat.ScopeUserRead
