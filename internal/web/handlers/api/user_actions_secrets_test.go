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

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/nacl/box"

	"github.com/tenseleyFlow/shithub/internal/actions/secrets"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
)

// PRO-EXT01-12b: integration tests for /api/v1/user/actions/secrets and
// runner-side resolution of user-scope secrets.

func TestUserActionsSecrets_PutListGetDeleteRoundTrip(t *testing.T) {
	env := newSecretsTestEnv(t)

	// Fetch public key.
	pubReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/public-key", nil)
	pubReq.Header.Set("Authorization", "Bearer "+env.userTokenRO)
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
	putReq.Header.Set("Authorization", "Bearer "+env.userTokenRW)
	putReq.Header.Set("Content-Type", "application/json")
	putRR := httptest.NewRecorder()
	env.router.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusNoContent {
		t.Fatalf("PUT status: %d body=%s", putRR.Code, putRR.Body.String())
	}

	// LIST exposes the name, never plaintext.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	listReq.Header.Set("Authorization", "Bearer "+env.userTokenRO)
	listRR := httptest.NewRecorder()
	env.router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusOK {
		t.Fatalf("LIST status: %d body=%s", listRR.Code, listRR.Body.String())
	}
	var listEnv struct {
		Secrets []map[string]any `json:"secrets"`
	}
	_ = json.Unmarshal(listRR.Body.Bytes(), &listEnv)
	listed := listEnv.Secrets
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
	delReq.Header.Set("Authorization", "Bearer "+env.userTokenRW)
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
	bobToken := mintRunnerAPIPAT(t, env.pool, bobID, string(pat.ScopeUserRead))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+bobToken)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bob LIST: %d body=%s", rr.Code, rr.Body.String())
	}
	var bobEnv struct {
		Secrets []map[string]any `json:"secrets"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &bobEnv)
	if len(bobEnv.Secrets) != 0 {
		t.Errorf("bob should see no secrets; got %+v", bobEnv.Secrets)
	}
}

// PRO-EXT_SR-01: a PAT bound to a single repo (via PRO-EXT01-11b) is
// not authorized for user-scope endpoints. The binding restricts a
// token to one repo; user-scope resources span every repo the user
// owns. Honoring a bound token here silently expands its blast
// radius beyond what the user authorized at mint time.

// mintBoundPAT inserts a PAT scoped to scopes and bound to repoID.
// Mirrors mintRunnerAPIPAT but with the RepoID field populated.
func mintBoundPAT(t *testing.T, pool *pgxpool.Pool, userID, repoID int64, scopes ...string) string {
	t.Helper()
	raw, hash, prefix, err := pat.Mint()
	if err != nil {
		t.Fatalf("pat.Mint: %v", err)
	}
	if _, err := usersdb.New().InsertUserToken(context.Background(), pool, usersdb.InsertUserTokenParams{
		UserID:      userID,
		Name:        "bound-pat",
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      scopes,
		RepoID:      pgtype.Int8{Int64: repoID, Valid: true},
	}); err != nil {
		t.Fatalf("InsertUserToken (bound): %v", err)
	}
	return raw
}

func TestUserActionsSecrets_BoundPATRejected(t *testing.T) {
	env := newSecretsTestEnv(t)
	// Bound PAT carries both user scopes so it gets past the scope
	// middleware and reaches the handler-level binding check — that's
	// the assertion this test pins. Without the right scopes the 403
	// would come from scope rejection instead of binding rejection.
	boundToken := mintBoundPAT(t, env.pool, env.userID, env.repoID,
		string(pat.ScopeUserRead), string(pat.ScopeUserWrite))

	// LIST: a bound token should be denied even on the read path.
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	listReq.Header.Set("Authorization", "Bearer "+boundToken)
	listRR := httptest.NewRecorder()
	env.router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusForbidden {
		t.Fatalf("bound PAT LIST: got %d, want 403; body=%s", listRR.Code, listRR.Body.String())
	}
	if !bytes.Contains(listRR.Body.Bytes(), []byte("bound to a single repo")) {
		t.Errorf("403 message should explain the binding; got %s", listRR.Body.String())
	}

	// PUT: the security-critical surface — a bound write-token must
	// not be able to mint a user-scope secret.
	pk := fetchUserSecretsPublicKey(t, env)
	putBody := sealPutBody(t, pk, []byte("should-not-land"))
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/user/actions/secrets/PWNED", bytes.NewReader(putBody))
	putReq.Header.Set("Authorization", "Bearer "+boundToken)
	putReq.Header.Set("Content-Type", "application/json")
	putRR := httptest.NewRecorder()
	env.router.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusForbidden {
		t.Fatalf("bound PAT PUT: got %d, want 403; body=%s", putRR.Code, putRR.Body.String())
	}

	// DELETE: the third mutation path.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/v1/user/actions/secrets/ANY", nil)
	delReq.Header.Set("Authorization", "Bearer "+boundToken)
	delRR := httptest.NewRecorder()
	env.router.ServeHTTP(delRR, delReq)
	if delRR.Code != http.StatusForbidden {
		t.Fatalf("bound PAT DELETE: got %d, want 403; body=%s", delRR.Code, delRR.Body.String())
	}

	// Public-key endpoint is also a probe surface — deny.
	pkReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/public-key", nil)
	pkReq.Header.Set("Authorization", "Bearer "+boundToken)
	pkRR := httptest.NewRecorder()
	env.router.ServeHTTP(pkRR, pkReq)
	if pkRR.Code != http.StatusForbidden {
		t.Fatalf("bound PAT public-key: got %d, want 403", pkRR.Code)
	}
}

func TestUserActionsSecrets_UnboundPATAccepted(t *testing.T) {
	// Regression guard so the bound-rejection check doesn't over-rotate
	// and break the unbound case.
	env := newSecretsTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	req.Header.Set("Authorization", "Bearer "+env.userTokenRO)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unbound PAT LIST: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// fetchUserSecretsPublicKey + sealPutBody are tiny test helpers — the
// existing PutListGetDeleteRoundTrip test inlines this exact shape;
// factoring it out keeps the new test focused.
func fetchUserSecretsPublicKey(t *testing.T, env *secretsTestEnv) [32]byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/public-key", nil)
	req.Header.Set("Authorization", "Bearer "+env.userTokenRO)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("fetch public key: %d body=%s", rr.Code, rr.Body.String())
	}
	var pk apiSecretsPublicKey
	_ = json.Unmarshal(rr.Body.Bytes(), &pk)
	raw, _ := base64.StdEncoding.DecodeString(pk.Key)
	var out [32]byte
	copy(out[:], raw)
	return out
}

func sealPutBody(t *testing.T, pubKey [32]byte, plaintext []byte) []byte {
	t.Helper()
	sealed, err := box.SealAnonymous(nil, plaintext, &pubKey, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	body, _ := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString(sealed),
	})
	return body
}

// PRO-EXT_SR-08: the REST GET-by-name path uses GetMeta (direct lookup)
// rather than List + linear scan. Functionally identical from the
// caller's perspective; this test pins the success path so a regression
// that reverts to List-and-filter would still pass here but the bench
// (BenchmarkResolveVisibleSecrets_UserScope) would regress.
func TestUserActionsSecrets_GetByNameReturnsMeta(t *testing.T) {
	env := newSecretsTestEnv(t)
	// Seed two secrets so a list-scan implementation would have a
	// reason to do real work.
	deps := secrets.Deps{Pool: env.pool, Box: env.secretBox}
	if err := deps.Set(context.Background(), secrets.UserScope(env.userID), "ALPHA", []byte("a"), env.userID); err != nil {
		t.Fatalf("seed ALPHA: %v", err)
	}
	if err := deps.Set(context.Background(), secrets.UserScope(env.userID), "BETA", []byte("b"), env.userID); err != nil {
		t.Fatalf("seed BETA: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/ALPHA", nil)
	req.Header.Set("Authorization", "Bearer "+env.userTokenRO)
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET by name: %d body=%s", rr.Code, rr.Body.String())
	}
	var meta map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &meta)
	if meta["name"] != "ALPHA" {
		t.Errorf("name: got %v want ALPHA", meta["name"])
	}
	if _, leaked := meta["value"]; leaked {
		t.Errorf("plaintext leaked from GET-by-name: %+v", meta)
	}

	// Missing → 404.
	missingReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/MISSING", nil)
	missingReq.Header.Set("Authorization", "Bearer "+env.userTokenRO)
	missingRR := httptest.NewRecorder()
	env.router.ServeHTTP(missingRR, missingReq)
	if missingRR.Code != http.StatusNotFound {
		t.Fatalf("missing GET: %d, want 404; body=%s", missingRR.Code, missingRR.Body.String())
	}
}

// PRO-EXT_SR-06: a PAT scoped only to repo:read/repo:write must not
// access the user-scope Actions secrets surface. The scope split
// makes "what scopes does this token need?" answerable by the
// resource kind (user-scope → user scopes) rather than the broad
// repo-scope catch-all.
func TestUserActionsSecrets_RepoScopePATRejected(t *testing.T) {
	env := newSecretsTestEnv(t)

	// LIST with repo:read — should be 403 (the user-scope group now
	// requires user:read).
	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets", nil)
	listReq.Header.Set("Authorization", "Bearer "+env.tokenRO)
	listRR := httptest.NewRecorder()
	env.router.ServeHTTP(listRR, listReq)
	if listRR.Code != http.StatusForbidden {
		t.Fatalf("repo:read LIST: got %d, want 403; body=%s", listRR.Code, listRR.Body.String())
	}

	// PUT with repo:write — should be 403 (the user-scope write group
	// now requires user:write).
	pk := fetchUserSecretsPublicKey(t, env)
	putBody := sealPutBody(t, pk, []byte("should-not-land"))
	putReq := httptest.NewRequest(http.MethodPut, "/api/v1/user/actions/secrets/SR06_REPO_SCOPE", bytes.NewReader(putBody))
	putReq.Header.Set("Authorization", "Bearer "+env.tokenRW)
	putReq.Header.Set("Content-Type", "application/json")
	putRR := httptest.NewRecorder()
	env.router.ServeHTTP(putRR, putReq)
	if putRR.Code != http.StatusForbidden {
		t.Fatalf("repo:write PUT: got %d, want 403; body=%s", putRR.Code, putRR.Body.String())
	}
}

// PRO-EXT_SR-05: the userActionsSecretsAPIGate's enforce-on / deny
// branch and Pro-user / accept branch were never exercised because
// the test env always set BillingEnforce.UserActionsSecrets=false.
// These tests pin all three combinations of (plan × enforce) the
// PUT path's gate must distinguish.

// putSealedUserSecret is the boilerplate the SR-05 tests share:
// fetch public key, seal the plaintext, PUT.
func putSealedUserSecret(t *testing.T, env *secretsTestEnv, name string, plaintext []byte) *httptest.ResponseRecorder {
	t.Helper()
	pk := fetchUserSecretsPublicKey(t, env)
	body := sealPutBody(t, pk, plaintext)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/user/actions/secrets/"+name, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.userTokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	return rr
}

func TestUserActionsSecrets_FreeUserPUTRejectedUnderEnforce(t *testing.T) {
	env := newSecretsTestEnvOpts(t, secretsTestEnvOptions{
		BillingEnforce: config.EnforceConfig{UserActionsSecrets: true},
	})
	rr := putSealedUserSecret(t, env, "SR05_FREE_ENFORCE", []byte("should-not-land"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("Free + enforce-on PUT: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("Pro")) {
		t.Errorf("error body should mention Pro upgrade: %s", rr.Body.String())
	}
}

func TestUserActionsSecrets_ProUserPUTAcceptedUnderEnforce(t *testing.T) {
	env := newSecretsTestEnvOpts(t, secretsTestEnvOptions{
		BillingEnforce: config.EnforceConfig{UserActionsSecrets: true},
	})
	upgradeUserToActivePro(t, env.pool, env.userID)
	rr := putSealedUserSecret(t, env, "SR05_PRO_ENFORCE", []byte("pro-value"))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("Pro + enforce-on PUT: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}

func TestUserActionsSecrets_FreeUserPUTAcceptedUnderReportOnly(t *testing.T) {
	env := newSecretsTestEnv(t)
	// BillingEnforce.UserActionsSecrets stays false (the campaign's
	// default soak-window state).
	rr := putSealedUserSecret(t, env, "SR05_FREE_REPORTONLY", []byte("ro-value"))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("Free + report-only PUT: got %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
}
