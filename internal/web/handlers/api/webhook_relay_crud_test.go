// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

// PRO-EXT01-13c: integration tests for /api/v1/user/webhook-relays/*.
// Reuses the relayEnv test fixture from webhook_relay_test.go.

// mintUserScopePAT returns a token with user:read + user:write — the
// scopes the CRUD endpoints require post-SR-06.
func mintUserScopePAT(t *testing.T, pool *pgxpool.Pool, userID int64) string {
	t.Helper()
	return mintRunnerAPIPAT(t, pool, userID,
		string(pat.ScopeUserRead), string(pat.ScopeUserWrite))
}

func TestUserWebhookRelaysCRUD_CreateListDelete(t *testing.T) {
	env := newRelayEnv(t, false)
	token := mintUserScopePAT(t, env.pool, env.userID)

	// CREATE.
	createBody, _ := json.Marshal(map[string]any{
		"name": "github-mirror",
		"destinations": []map[string]any{
			{"url": "http://127.0.0.1:8023/dev"},
		},
	})
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/user/webhook-relays", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer "+token)
	createReq.Header.Set("Content-Type", "application/json")
	createRR := httptest.NewRecorder()
	env.router.ServeHTTP(createRR, createReq)
	if createRR.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201; body=%s", createRR.Code, createRR.Body.String())
	}
	var created map[string]any
	_ = json.Unmarshal(createRR.Body.Bytes(), &created)
	if created["token"] == nil || created["token"] == "" {
		t.Errorf("response should include one-shot raw token: %+v", created)
	}
	id := int64(created["id"].(float64))

	// LIST.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/webhook-relays", nil)
	listReq.Header.Set("Authorization", "Bearer "+token)
	listRR := httptest.NewRecorder()
	env.router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: %d body=%s", listRR.Code, listRR.Body.String())
	}
	var listed []map[string]any
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0]["name"] != "github-mirror" {
		t.Errorf("list shape: %+v", listed)
	}
	if _, leaked := listed[0]["token"]; leaked {
		t.Errorf("token leaked into list: %+v", listed[0])
	}

	// DELETE.
	delURL := "/api/v1/user/webhook-relays/" + jsonInt64(id)
	delReq := httptest.NewRequest(http.MethodDelete, delURL, nil)
	delReq.Header.Set("Authorization", "Bearer "+token)
	delRR := httptest.NewRecorder()
	env.router.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("delete: got %d, want 204", delRR.Code)
	}

	// LIST again — empty.
	listRR2 := httptest.NewRecorder()
	env.router.ServeHTTP(listRR2, listReq)
	var listed2 []map[string]any
	_ = json.Unmarshal(listRR2.Body.Bytes(), &listed2)
	if len(listed2) != 0 {
		t.Errorf("list after delete: got %+v, want []", listed2)
	}
}

func TestUserWebhookRelaysCRUD_RepoScopeRejected(t *testing.T) {
	env := newRelayEnv(t, false)
	// Repo-scope PAT must not reach user-scope endpoints (SR-06 contract).
	repoToken := mintRunnerAPIPAT(t, env.pool, env.userID, string(pat.ScopeRepoRead))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/webhook-relays", nil)
	req.Header.Set("Authorization", "Bearer "+repoToken)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("repo:read on user surface: got %d, want 403", rr.Code)
	}
}

func TestUserWebhookRelaysCRUD_DeleteNonOwnedReturns404(t *testing.T) {
	env := newRelayEnv(t, false)
	_, otherID := env.seedRelay(t, webhookrelay.Destination{URL: "http://127.0.0.1:8022/x"})

	// A different user with a valid PAT tries to delete alice's relay.
	bobID := seedRepoCreatorUser(t, env.pool, "bob")
	bobToken := mintUserScopePAT(t, env.pool, bobID)
	url := "/api/v1/user/webhook-relays/" + jsonInt64(otherID)
	req := httptest.NewRequest(http.MethodDelete, url, nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-owner delete: got %d, want 404", rr.Code)
	}
}

func TestUserWebhookRelaysCRUD_FreeUserEnforceCreateReturns403(t *testing.T) {
	env := newRelayEnv(t, true) // enforce on
	token := mintUserScopePAT(t, env.pool, env.userID)
	body, _ := json.Marshal(map[string]any{"name": "x", "destinations": []any{}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/webhook-relays", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("Free + enforce create: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

// jsonInt64 stringifies an int64 for URL building. JSON's number type
// unmarshals to float64, so id round-trips need this bridge.
func jsonInt64(id int64) string { return strconv.FormatInt(id, 10) }
