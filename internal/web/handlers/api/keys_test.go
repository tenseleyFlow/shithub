// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

type apiUserKey struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Key         string `json:"key"`
	Fingerprint string `json:"fingerprint"`
	KeyType     string `json:"key_type"`
	Verified    bool   `json:"verified"`
	ReadOnly    bool   `json:"read_only"`
	CreatedAt   string `json:"created_at"`
}

func loadKeyFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "auth", "sshkey", "testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func seedSigningKey(t *testing.T, pool *pgxpool.Pool, userID int64, fingerprint string) int64 {
	t.Helper()
	k, err := usersdb.New().InsertUserSSHKey(context.Background(), pool, usersdb.InsertUserSSHKeyParams{
		UserID:            userID,
		Title:             "signing-only",
		FingerprintSha256: fingerprint,
		KeyType:           "ssh-ed25519",
		KeyBits:           0,
		PublicKey:         "ssh-ed25519 AAAA…signing test",
		Kind:              "signing",
	})
	if err != nil {
		t.Fatalf("seed signing key: %v", err)
	}
	return k.ID
}

func TestUserKeys_CreateListGetDelete(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	tokenWrite := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))
	tokenRead := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	pubBlob := loadKeyFixture(t, "ed25519.pub")

	// Create.
	body, _ := json.Marshal(map[string]string{"title": "laptop", "key": pubBlob})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create status: got %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var created apiUserKey
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v; body=%s", err, rr.Body.String())
	}
	if created.ID == 0 || created.Title != "laptop" || created.KeyType != "ssh-ed25519" {
		t.Errorf("created shape unexpected: %+v", created)
	}
	if !strings.HasPrefix(created.Fingerprint, "SHA256:") {
		t.Errorf("fingerprint not prefixed: %q", created.Fingerprint)
	}
	if strings.Count(created.Fingerprint, "SHA256:") != 1 {
		t.Errorf("fingerprint prefix count = %d, want 1: %q",
			strings.Count(created.Fingerprint, "SHA256:"), created.Fingerprint)
	}

	// List.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/keys", nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiUserKey
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("list shape unexpected: %+v", listed)
	}
	if listed[0].Fingerprint != created.Fingerprint {
		t.Errorf("list fingerprint = %q, want %q", listed[0].Fingerprint, created.Fingerprint)
	}

	// Get by id.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got apiUserKey
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get: %v; body=%s", err, rr.Body.String())
	}
	if got.Fingerprint != created.Fingerprint {
		t.Errorf("get fingerprint = %q, want %q", got.Fingerprint, created.Fingerprint)
	}

	// Delete.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/user/keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// Subsequent get returns 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("post-delete get: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserKeys_CreateRejectsBadKey(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]string{"title": "broken", "key": "not even close to a key"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Error == "" {
		t.Errorf("error envelope empty: %s", rr.Body.String())
	}
}

func TestUserKeys_CreateRejectsRSA1024(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]string{"title": "weak", "key": loadKeyFixture(t, "rsa1024.pub")})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserKeys_CreateRequiresUserWriteScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))
	pubBlob := loadKeyFixture(t, "ed25519.pub")

	body, _ := json.Marshal(map[string]string{"title": "laptop", "key": pubBlob})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserKeys_GetUnknownReturns404(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	token := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/keys/9999999", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserKeys_ListExcludesSigningKeys(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	tokenWrite := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))
	tokenRead := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	// Add an authentication key via REST.
	pubBlob := loadKeyFixture(t, "ed25519.pub")
	body, _ := json.Marshal(map[string]string{"title": "laptop", "key": pubBlob})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed auth key: %d; body=%s", rr.Code, rr.Body.String())
	}

	// Seed a signing key directly.
	signingID := seedSigningKey(t, pool, userID, "z9R6V9d8aGAxN1pVdQF/notarealfingerprint=")

	// List should return only the authentication one.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/keys", nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list status: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var listed []apiUserKey
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, k := range listed {
		if k.ID == signingID {
			t.Errorf("signing key leaked into auth-keys list: %+v", k)
		}
	}

	// Direct GET of the signing key by id should also 404 on this surface.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/keys/"+itoa(signingID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("signing key direct get: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserKeys_DeleteOnlyOwnersKey(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)

	// User A creates a key.
	userA := crossCuttingUser(t, pool)
	tokenA := mintRunnerAPIPAT(t, pool, userA, string(pat.ScopeUserWrite))
	pubBlob := loadKeyFixture(t, "ed25519.pub")
	body, _ := json.Marshal(map[string]string{"title": "laptop", "key": pubBlob})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenA)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("A create: %d; %s", rr.Code, rr.Body.String())
	}
	var created apiUserKey
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// User B tries to delete user A's key.
	userB, err := usersdb.New().CreateUser(context.Background(), pool, usersdb.CreateUserParams{
		Username:     "bob",
		DisplayName:  "Bob",
		PasswordHash: runnerAPIFixtureHash,
	})
	if err != nil {
		t.Fatalf("create userB: %v", err)
	}
	tokenB := mintRunnerAPIPAT(t, pool, userB.ID, string(pat.ScopeUserWrite))
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/user/keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenB)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("cross-user delete: got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func itoa(i int64) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
