// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
)

// apiGPGKey mirrors the response shape produced by presentGPGKey.
// We only declare the fields the tests actually assert against; gh's
// can_authenticate is intentionally absent (not in the wire response).
type apiGPGKey struct {
	ID                int64          `json:"id"`
	Name              string         `json:"name"`
	PrimaryKeyID      *int64         `json:"primary_key_id"`
	KeyID             string         `json:"key_id"`
	PublicKey         string         `json:"public_key"`
	RawKey            string         `json:"raw_key"`
	Emails            []apiGPGEmail  `json:"emails"`
	Subkeys           []apiGPGSubkey `json:"subkeys"`
	CanSign           bool           `json:"can_sign"`
	CanEncryptComms   bool           `json:"can_encrypt_comms"`
	CanEncryptStorage bool           `json:"can_encrypt_storage"`
	CanCertify        bool           `json:"can_certify"`
	Revoked           bool           `json:"revoked"`
}

type apiGPGEmail struct {
	Email    string `json:"email"`
	Verified bool   `json:"verified"`
}

type apiGPGSubkey struct {
	KeyID             string `json:"key_id"`
	PrimaryKeyID      int64  `json:"primary_key_id"`
	CanSign           bool   `json:"can_sign"`
	CanEncryptComms   bool   `json:"can_encrypt_comms"`
	CanEncryptStorage bool   `json:"can_encrypt_storage"`
}

// gpgArmoredPublic synthesizes an in-memory ed25519 entity and
// returns its armored public-key block.
func gpgArmoredPublic(t *testing.T, email string) string {
	t.Helper()
	e, err := openpgp.NewEntity("test", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	if err != nil {
		t.Fatalf("armor.Encode: %v", err)
	}
	if err := e.Serialize(w); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	_ = w.Close()
	return buf.String()
}

// gpgEncryptOnlyPublic builds a rsa2048 entity with only encryption
// capability flags on the primary's self-sig — the gh-parity
// fixture asserting we accept encrypt-only keys.
func gpgEncryptOnlyPublic(t *testing.T, email string) string {
	t.Helper()
	e, err := openpgp.NewEntity("eo", "", email, &packet.Config{
		Algorithm: packet.PubKeyAlgoRSA, RSABits: 2048,
	})
	if err != nil {
		t.Fatalf("NewEntity: %v", err)
	}
	for _, id := range e.Identities {
		id.SelfSignature.FlagSign = false
		id.SelfSignature.FlagCertify = false
		id.SelfSignature.FlagEncryptCommunications = true
		id.SelfSignature.FlagEncryptStorage = true
		if err := id.SelfSignature.SignUserId(id.UserId.Id, e.PrimaryKey, e.PrivateKey, nil); err != nil {
			t.Fatalf("re-sign: %v", err)
		}
	}
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, "PGP PUBLIC KEY BLOCK", nil)
	_ = e.Serialize(w)
	_ = w.Close()
	return buf.String()
}

// seedUserEmail inserts a single email row for the user so the
// orchestrator's verified-email cross-check has something to find.
func seedUserEmail(t *testing.T, pool *pgxpool.Pool, userID int64, email string, verified bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO user_emails (user_id, email, verified) VALUES ($1, $2, $3)`,
		userID, email, verified,
	); err != nil {
		t.Fatalf("seed user_email: %v", err)
	}
}

// crossCuttingNamedUser is the named-arg variant of crossCuttingUser
// — useful when a test needs two distinct users (alice + bob) on the
// same pool to exercise cross-user isolation.
func crossCuttingNamedUser(t *testing.T, pool *pgxpool.Pool, username string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (username, password_hash, email_verified)
		 VALUES ($1, 'x', true) RETURNING id`,
		username,
	).Scan(&id)
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return id
}

func TestGPGKeys_CreateListGetDelete(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	seedUserEmail(t, pool, userID, "alice@shithub.test", true)
	tokenWrite := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))
	tokenRead := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	armored := gpgArmoredPublic(t, "alice@shithub.test")
	body, _ := json.Marshal(map[string]string{
		"name":               "laptop",
		"armored_public_key": armored,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created apiGPGKey
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.Name != "laptop" {
		t.Errorf("Name: got %q, want laptop", created.Name)
	}
	if created.PublicKey == "" || created.RawKey == "" {
		t.Error("PublicKey + RawKey should both be populated")
	}
	if created.PublicKey != created.RawKey {
		t.Error("PublicKey and RawKey should carry the same armored block")
	}
	if !created.CanSign && !created.CanCertify {
		t.Error("expected sign or certify on synthesized ed25519 key")
	}
	if len(created.Emails) == 0 || created.Emails[0].Email != "alice@shithub.test" {
		t.Errorf("expected alice@shithub.test in emails; got %+v", created.Emails)
	}
	if !created.Emails[0].Verified {
		t.Error("expected emails[0].verified=true (cross-checked against user_emails)")
	}

	// List with the user:read PAT.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/gpg_keys", nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var listed []apiGPGKey
	if err := json.Unmarshal(rr.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Errorf("list: got %+v, want [the new key]", listed)
	}

	// Single GET with user:read.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/gpg_keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}

	// Delete with user:write → 204.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/user/gpg_keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
	}

	// Subsequent GET → 404 (revoked rows aren't visible on the
	// user-scoped GET).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/gpg_keys/"+itoa(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("post-delete GET: %d, want 404", rr.Code)
	}
}

func TestGPGKeys_CreateRequiresUserWriteScope(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	tokenRead := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserRead))

	body, _ := json.Marshal(map[string]string{
		"armored_public_key": gpgArmoredPublic(t, "alice@shithub.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenRead)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("create with read-only PAT: %d, want 403", rr.Code)
	}
}

func TestGPGKeys_CreateAcceptsEncryptOnly(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	tokenWrite := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	body, _ := json.Marshal(map[string]string{
		"armored_public_key": gpgEncryptOnlyPublic(t, "encryptonly@shithub.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("encrypt-only create: %d %s; gh parity says accept", rr.Code, rr.Body.String())
	}
	var created apiGPGKey
	_ = json.Unmarshal(rr.Body.Bytes(), &created)
	if created.CanSign {
		t.Error("expected can_sign=false on encryption-only key")
	}
	if !created.CanEncryptComms && !created.CanEncryptStorage {
		t.Error("expected at least one encrypt-* true on encryption-only key")
	}
}

func TestGPGKeys_CreateRejectsPrivateKeyBlock(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	tokenWrite := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	// Build a private-key armored block via SerializePrivate.
	e, _ := openpgp.NewEntity("priv", "", "priv@shithub.test", &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	})
	var buf bytes.Buffer
	w, _ := armor.Encode(&buf, "PGP PRIVATE KEY BLOCK", nil)
	_ = e.SerializePrivate(w, nil)
	_ = w.Close()

	body, _ := json.Marshal(map[string]string{"armored_public_key": buf.String()})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("private key block: %d %s, want 422", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "private key") {
		t.Errorf("error body missing 'private key' phrase: %s", rr.Body.String())
	}
}

func TestGPGKeys_CrossUserGetReturns404(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	aliceID := crossCuttingUser(t, pool)
	seedUserEmail(t, pool, aliceID, "alice@shithub.test", true)
	aliceWrite := mintRunnerAPIPAT(t, pool, aliceID, string(pat.ScopeUserWrite))

	// Alice adds a key.
	body, _ := json.Marshal(map[string]string{
		"armored_public_key": gpgArmoredPublic(t, "alice@shithub.test"),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+aliceWrite)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("alice add: %d %s", rr.Code, rr.Body.String())
	}
	var alicesKey apiGPGKey
	_ = json.Unmarshal(rr.Body.Bytes(), &alicesKey)

	// Bob tries to GET alice's key by id.
	bobID := crossCuttingNamedUser(t, pool, "bob")
	bobRead := mintRunnerAPIPAT(t, pool, bobID, string(pat.ScopeUserRead))
	req = httptest.NewRequest(http.MethodGet, "/api/v1/user/gpg_keys/"+itoa(alicesKey.ID), nil)
	req.Header.Set("Authorization", "Bearer "+bobRead)
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("cross-user GET: %d, want 404 (existence-leak-safe)", rr.Code)
	}
}

func TestGPGKeys_DuplicateFingerprintReturns422(t *testing.T) {
	pool := dbtest.NewTestDB(t)
	router := newCrossCuttingAPIRouter(t, pool)
	userID := crossCuttingUser(t, pool)
	tokenWrite := mintRunnerAPIPAT(t, pool, userID, string(pat.ScopeUserWrite))

	armored := gpgArmoredPublic(t, "alice@shithub.test")
	body, _ := json.Marshal(map[string]string{"armored_public_key": armored})

	// First add succeeds.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("first add: %d", rr.Code)
	}

	// Second add of the same fingerprint → 422.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/user/gpg_keys", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tokenWrite)
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate add: %d %s, want 422", rr.Code, rr.Body.String())
	}
}
