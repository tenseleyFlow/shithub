// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/nacl/box"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/sealbox"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	reposdb "github.com/tenseleyFlow/shithub/internal/repos/sqlc"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

type apiSecretsPublicKey struct {
	KeyID string `json:"key_id"`
	Key   string `json:"key"`
}

// secretsTestEnv stands up an api router with both the storage AEAD
// box and the sealed-box keypair wired in. The returned `secretBox`
// is the storage AEAD, exposed so tests can decrypt directly and
// assert the round-trip lands the actual plaintext on disk.
type secretsTestEnv struct {
	pool      *pgxpool.Pool
	router    http.Handler
	secretBox *secretbox.Box
	repoID    int64
	userID    int64
	owner     string
	repoName  string
	tokenRO   string
	tokenRW   string
}

func newSecretsTestEnv(t *testing.T) *secretsTestEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	storageKey, err := secretbox.GenerateKey()
	if err != nil {
		t.Fatalf("secretbox key: %v", err)
	}
	sBox, err := secretbox.FromBytes(storageKey)
	if err != nil {
		t.Fatalf("secretbox: %v", err)
	}
	pkBox, err := sealbox.New()
	if err != nil {
		t.Fatalf("sealbox: %v", err)
	}

	h, err := apih.New(apih.Deps{
		Pool:        pool,
		Logger:      logger,
		RepoFS:      rfs,
		SecretBox:   sBox,
		SecretsBox:  pkBox,
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
		t.Fatalf("apih.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)

	userID := seedRepoCreatorUser(t, pool, "alice")
	owner, repoName := "alice", "demo"
	row, err := reposdb.New().CreateRepo(context.Background(), pool, reposdb.CreateRepoParams{
		Name:          repoName,
		OwnerUserID:   pgtype.Int8{Int64: userID, Valid: true},
		Visibility:    reposdb.RepoVisibilityPublic,
		DefaultBranch: "trunk",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	return &secretsTestEnv{
		pool:      pool,
		router:    r,
		secretBox: sBox,
		repoID:    row.ID,
		userID:    userID,
		owner:     owner,
		repoName:  repoName,
		tokenRO:   mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoRead)),
		tokenRW:   mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeRepoWrite)),
	}
}

func TestActionsSecrets_PublicKeyEndpoint(t *testing.T) {
	env := newSecretsTestEnv(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/secrets/public-key", nil)
	req.Header.Set("Authorization", "Bearer "+env.tokenRO)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var got apiSecretsPublicKey
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.KeyID == "" || got.Key == "" {
		t.Errorf("missing fields: %+v", got)
	}
	// Public key must base64-decode to 32 bytes.
	raw, err := base64.StdEncoding.DecodeString(got.Key)
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if len(raw) != 32 {
		t.Errorf("key length: got %d, want 32", len(raw))
	}
}

func TestActionsSecrets_PutGetDeleteRoundTrip(t *testing.T) {
	env := newSecretsTestEnv(t)

	pubReq := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/secrets/public-key", nil)
	pubReq.Header.Set("Authorization", "Bearer "+env.tokenRO)
	pubRR := httptest.NewRecorder()
	env.router.ServeHTTP(pubRR, pubReq)
	var pk apiSecretsPublicKey
	_ = json.Unmarshal(pubRR.Body.Bytes(), &pk)

	pubBytes, _ := base64.StdEncoding.DecodeString(pk.Key)
	var pubKey [32]byte
	copy(pubKey[:], pubBytes)

	plaintext := []byte("supersecret-value")
	sealed, err := box.SealAnonymous(nil, plaintext, &pubKey, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	body, _ := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString(sealed),
		"key_id":          pk.KeyID,
	})

	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/secrets/MY_TOKEN", bytes.NewReader(body))
	putReq.Header.Set("Authorization", "Bearer "+env.tokenRW)
	putReq.Header.Set("Content-Type", "application/json")
	putRR := httptest.NewRecorder()
	env.router.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusNoContent {
		t.Fatalf("PUT status: got %d; body=%s", putRR.Code, putRR.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/secrets", nil)
	listReq.Header.Set("Authorization", "Bearer "+env.tokenRO)
	listRR := httptest.NewRecorder()
	env.router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("LIST status: got %d; body=%s", listRR.Code, listRR.Body.String())
	}
	var listed []map[string]any
	_ = json.Unmarshal(listRR.Body.Bytes(), &listed)
	if len(listed) != 1 {
		t.Fatalf("expected 1 secret; got %+v", listed)
	}
	if name, _ := listed[0]["name"].(string); name != "MY_TOKEN" {
		t.Errorf("secret name: %+v", listed[0])
	}
	if _, leaked := listed[0]["value"]; leaked {
		t.Errorf("plaintext leaked into list response: %+v", listed[0])
	}

	// Runner-side decryption confirms the round-trip lands the actual
	// plaintext at rest in workflow_secrets.
	dec, err := secrets.Deps{Pool: env.pool, Box: env.secretBox}.Get(
		context.Background(), secrets.RepoScope(env.repoID), "MY_TOKEN")
	if err != nil {
		t.Fatalf("orchestrator Get: %v", err)
	}
	if string(dec) != string(plaintext) {
		t.Errorf("plaintext round-trip: got %q, want %q", dec, plaintext)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/repos/alice/demo/actions/secrets/MY_TOKEN", nil)
	delReq.Header.Set("Authorization", "Bearer "+env.tokenRW)
	delRR := httptest.NewRecorder()
	env.router.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusNoContent {
		t.Fatalf("DELETE status: got %d; body=%s", delRR.Code, delRR.Body.String())
	}
}

func TestActionsSecrets_StaleKeyIDRejected(t *testing.T) {
	env := newSecretsTestEnv(t)
	writeToken := env.tokenRW
	router := env.router

	body, _ := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString([]byte("doesnt-matter")),
		"key_id":          "stale-key-id-1234",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/secrets/X", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsSecrets_PutRejectsBadCiphertext(t *testing.T) {
	env := newSecretsTestEnv(t)
	writeToken := env.tokenRW
	router := env.router

	body, _ := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString([]byte("too-short")),
	})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/secrets/X", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+writeToken)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsSecrets_GetUnknown404(t *testing.T) {
	env := newSecretsTestEnv(t)
	token := env.tokenRO
	router := env.router
	req := httptest.NewRequest(http.MethodGet, "/api/v1/repos/alice/demo/actions/secrets/MISSING", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestActionsSecrets_PutRequiresRepoWrite(t *testing.T) {
	env := newSecretsTestEnv(t)
	token := env.tokenRO
	router := env.router
	body, _ := json.Marshal(map[string]string{"encrypted_value": base64.StdEncoding.EncodeToString([]byte("xx"))})
	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/alice/demo/actions/secrets/X", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token) // repo:read only
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}
