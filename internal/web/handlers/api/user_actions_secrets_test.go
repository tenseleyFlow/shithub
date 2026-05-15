// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/nacl/box"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
)

// PRO-EXT01-12b: integration tests for /api/v1/user/actions/secrets and
// runner-side resolution of user-scope secrets.

func TestUserActionsSecrets_PutListGetDeleteRoundTrip(t *testing.T) {
	env := newSecretsTestEnv(t)

	// Fetch public key.
	pubReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/public-key", nil)
	pubReq.Header.Set("Authorization", "Bearer "+env.tokenRO)
	pubRR := httptest.NewRecorder()
	env.router.ServeHTTP(pubRR, pubReq)
	if pubRR.Code != http.StatusOK {
		t.Fatalf("public-key status: %d body=%s", pubRR.Code, pubRR.Body.String())
	}
	var pk apiSecretsPublicKey
	_ = json.Unmarshal(pubRR.Body.Bytes(), &pk)
	pubBytes, _ := base64.StdEncoding.DecodeString(pk.Key)
	var pubKey [32]byte
	copy(pubKey[:], pubBytes)

	// PUT.
	plaintext := []byte("personal-secret-xyz")
	sealed, err := box.SealAnonymous(nil, plaintext, &pubKey, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	putBody, _ := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString(sealed),
		"key_id":          pk.KeyID,
	})
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/user/actions/secrets/PERSONAL_TOKEN", bytes.NewReader(putBody))
	putReq.Header.Set("Authorization", "Bearer "+env.tokenRW)
	putReq.Header.Set("Content-Type", "application/json")
	putRR := httptest.NewRecorder()
	env.router.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusNoContent {
		t.Fatalf("PUT status: %d body=%s", putRR.Code, putRR.Body.String())
	}

	// LIST exposes the name, never plaintext.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	listReq.Header.Set("Authorization", "Bearer "+env.tokenRO)
	listRR := httptest.NewRecorder()
	env.router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("LIST status: %d body=%s", listRR.Code, listRR.Body.String())
	}
	var listed []map[string]any
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 1 || listed[0]["name"] != "PERSONAL_TOKEN" {
		t.Fatalf("expected one PERSONAL_TOKEN; got %+v", listed)
	}
	if _, leaked := listed[0]["value"]; leaked {
		t.Errorf("plaintext leaked into list: %+v", listed[0])
	}

	// Round-trip the value via the orchestrator to confirm the actual
	// encrypted bytes match the input plaintext.
	dec, err := secrets.Deps{Pool: env.pool, Box: env.secretBox}.Get(
		context.Background(), secrets.UserScope(env.userID), "PERSONAL_TOKEN",
	)
	if err != nil {
		t.Fatalf("orchestrator Get: %v", err)
	}
	if string(dec) != string(plaintext) {
		t.Errorf("round-trip plaintext: got %q, want %q", dec, plaintext)
	}

	// DELETE.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/user/actions/secrets/PERSONAL_TOKEN", nil)
	delReq.Header.Set("Authorization", "Bearer "+env.tokenRW)
	delRR := httptest.NewRecorder()
	env.router.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE status: %d body=%s", delRR.Code, delRR.Body.String())
	}
}

func TestUserActionsSecrets_AnonymousRejected(t *testing.T) {
	env := newSecretsTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET: got %d, want 401", rr.Code)
	}
}

func TestUserActionsSecrets_OwnerOnlySeesOwnSecrets(t *testing.T) {
	env := newSecretsTestEnv(t)

	// Seed a personal secret for alice directly via the orchestrator.
	deps := secrets.Deps{Pool: env.pool, Box: env.secretBox}
	if err := deps.Set(context.Background(), secrets.UserScope(env.userID), "ALICE_SECRET", []byte("a"), env.userID); err != nil {
		t.Fatalf("seed alice secret: %v", err)
	}

	// Mint a token for a brand-new user bob and confirm bob's list is empty.
	bobID := seedRepoCreatorUser(t, env.pool, "bob")
	bobToken := mintRunnerAPIPAT(t, env.pool, bobID, string(pat.ScopeRepoRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob LIST: %d body=%s", rr.Code, rr.Body.String())
	}
	var listed []map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &listed)
	if len(listed) != 0 {
		t.Errorf("bob should see no secrets; got %+v", listed)
	}
}
