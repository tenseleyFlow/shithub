// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/nacl/box"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/pat"
	"github.com/tenseleyFlow/shithub/internal/auth/sealbox"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
)

// PRO-EXT_SR-02 — soak-window telemetry.
//
// Pin that the API gate emits `entitlements.report_only_deny` when a
// non-Pro user transits the gate, both under report-only (the soak
// window) and under enforce (post-flip staff-review signal).
//
// The matching runner-side assertion lives in the in-package test
// file (runner_user_secrets_test.go) because the runner resolver is
// unexported.

// telemetryEnv stands up an isolated API router with a captured
// logger so tests can assert what got emitted.
type telemetryEnv struct {
	router  http.Handler
	pool    *pgxpool.Pool
	userID  int64
	logBuf  *bytes.Buffer
	tokenRW string
}

func newTelemetryEnv(t *testing.T, enforce bool) *telemetryEnv {
	t.Helper()
	pool := dbtest.NewTestDB(t)
	rfs, err := storage.NewRepoFS(t.TempDir())
	if err != nil {
		t.Fatalf("NewRepoFS: %v", err)
	}
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	storageKey, _ := secretbox.GenerateKey()
	sBox, _ := secretbox.FromBytes(storageKey)
	pkBox, _ := sealbox.New()

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
			AuthedPerHour: 5000, AnonPerHour: 60,
			Logger: logger,
		},
		BillingEnforce: config.EnforceConfig{UserActionsSecrets: enforce},
	})
	if err != nil {
		t.Fatalf("apih.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	userID := seedRepoCreatorUser(t, pool, "alice-sr02")
	return &telemetryEnv{
		router:  r,
		pool:    pool,
		userID:  userID,
		logBuf:  logBuf,
		// PRO-EXT_SR-06: user-scope endpoints now require user:read /
		// user:write. Use a token holding both so this env can fetch the
		// public key (read) and PUT (write).
		tokenRW: mintRunnerAPIPAT(t, pool, userID,
			string(pat.ScopeUserRead), string(pat.ScopeUserWrite)),
	}
}

func (e *telemetryEnv) sealedBody(t *testing.T, plaintext []byte) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/user/actions/secrets/public-key", nil)
	req.Header.Set("Authorization", "Bearer "+e.tokenRW)
	rr := httptest.NewRecorder()
	e.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("public-key: %d body=%s", rr.Code, rr.Body.String())
	}
	var pk apiSecretsPublicKey
	_ = json.Unmarshal(rr.Body.Bytes(), &pk)
	rawKey, _ := base64.StdEncoding.DecodeString(pk.Key)
	var pubKey [32]byte
	copy(pubKey[:], rawKey)
	sealed, err := box.SealAnonymous(nil, plaintext, &pubKey, rand.Reader)
	if err != nil {
		t.Fatalf("SealAnonymous: %v", err)
	}
	body, _ := json.Marshal(map[string]string{
		"encrypted_value": base64.StdEncoding.EncodeToString(sealed),
	})
	return body
}

func TestUserActionsSecretsAPIGate_EmitsReportOnlyDenyForFreeUser(t *testing.T) {
	env := newTelemetryEnv(t, false)
	body := env.sealedBody(t, []byte("anything"))

	req := httptest.NewRequest(http.MethodPut, "/api/v1/user/actions/secrets/SR02_TOKEN", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("report-only PUT: %d body=%s", rr.Code, rr.Body.String())
	}

	out := env.logBuf.String()
	if !strings.Contains(out, `"msg":"entitlements.report_only_deny"`) {
		t.Errorf("expected report_only_deny log line; got=%s", out)
	}
	if !strings.Contains(out, `"feature":"user_actions_secrets"`) {
		t.Errorf("log line should name the feature: %s", out)
	}
	if !strings.Contains(out, `"surface":"api"`) {
		t.Errorf("log line should tag the surface: %s", out)
	}
	if !strings.Contains(out, `"mode":"report_only"`) {
		t.Errorf("log line should tag mode=report_only: %s", out)
	}
}

func TestUserActionsSecretsAPIGate_EmitsEnforceModeOnReject(t *testing.T) {
	env := newTelemetryEnv(t, true)
	body := env.sealedBody(t, []byte("anything"))

	req := httptest.NewRequest(http.MethodPut, "/api/v1/user/actions/secrets/SR02_TOKEN", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+env.tokenRW)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("enforce PUT: %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(env.logBuf.String(), `"mode":"enforce"`) {
		t.Errorf("enforce-mode tag missing: %s", env.logBuf.String())
	}
}
