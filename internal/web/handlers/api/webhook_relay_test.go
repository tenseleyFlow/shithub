// SPDX-License-Identifier: AGPL-3.0-or-later

package api_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/audit"
	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/auth/throttle"
	"github.com/tenseleyFlow/shithub/internal/infra/config"
	"github.com/tenseleyFlow/shithub/internal/infra/storage"
	"github.com/tenseleyFlow/shithub/internal/ratelimit"
	"github.com/tenseleyFlow/shithub/internal/testing/dbtest"
	apih "github.com/tenseleyFlow/shithub/internal/web/handlers/api"
	"github.com/tenseleyFlow/shithub/internal/web/handlers/api/apilimit"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
)

// PRO-EXT01-13a: receiver integration tests. The token in the URL is
// the only auth — no PAT, no session. These pin the documented HTTP
// behaviors that the security model depends on: 404 collapse for
// unknown/malformed, 410 for disabled, 413 for oversized, 403 for
// enforce-on Free user, log-emit for report-only Free user.

type relayEnv struct {
	router    http.Handler
	pool      *pgxpool.Pool
	secretBox *secretbox.Box
	userID    int64
	logBuf    *bytes.Buffer
}

func newRelayEnv(t *testing.T, enforce bool) *relayEnv {
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
		BillingEnforce: config.EnforceConfig{UserWebhookRelay: enforce},
	})
	if err != nil {
		t.Fatalf("apih.New: %v", err)
	}
	r := chi.NewRouter()
	h.Mount(r)
	userID := seedRepoCreatorUser(t, pool, "alice-relay")
	return &relayEnv{
		router:    r,
		pool:      pool,
		secretBox: sBox,
		userID:    userID,
		logBuf:    logBuf,
	}
}

// seedRelay creates a relay row via the store layer and returns the
// raw token + relay id. The token is the path param the receiver
// reads at `/webhook-relay/{token}`.
func (e *relayEnv) seedRelay(t *testing.T, dests ...webhookrelay.Destination) (string, int64) {
	t.Helper()
	res, err := (webhookrelay.Deps{Pool: e.pool, Box: e.secretBox}).Create(
		context.Background(), webhookrelay.CreateInput{
			UserID: e.userID, Name: "test-relay", HMACSecret: []byte("k"),
			Destinations: dests,
		},
	)
	if err != nil {
		t.Fatalf("seed relay: %v", err)
	}
	return res.RawToken, res.ID
}

func TestRelayReceiver_HappyPathReturns202(t *testing.T) {
	env := newRelayEnv(t, false)
	token, _ := env.seedRelay(t, webhookrelay.Destination{URL: "https://dest.example.test/"})
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/"+token,
		bytes.NewReader([]byte(`{"x":1}`)))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("X-Shithub-Relay-Request") == "" {
		t.Error("X-Shithub-Relay-Request header should be set")
	}
}

func TestRelayReceiver_UnknownTokenReturns404(t *testing.T) {
	env := newRelayEnv(t, false)
	// Mint a fresh, never-inserted token (correct shape).
	raw, _, _, err := webhookrelay.Mint()
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/"+raw,
		bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rr.Code)
	}
}

func TestRelayReceiver_MalformedTokenAlsoReturns404(t *testing.T) {
	// Malformed shouldn't differ from unknown — keeps probe behavior
	// uniform so an attacker can't tell good shape from existing token.
	env := newRelayEnv(t, false)
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/garbage",
		bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rr.Code)
	}
}

func TestRelayReceiver_DisabledRelayReturns410(t *testing.T) {
	env := newRelayEnv(t, false)
	token, id := env.seedRelay(t, webhookrelay.Destination{URL: "https://dest.example.test/"})
	if err := (webhookrelay.Deps{Pool: env.pool, Box: env.secretBox}).Disable(
		context.Background(), id,
	); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/"+token,
		bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusGone {
		t.Fatalf("status: got %d, want 410", rr.Code)
	}
}

func TestRelayReceiver_OversizedBodyReturns413(t *testing.T) {
	env := newRelayEnv(t, false)
	token, _ := env.seedRelay(t, webhookrelay.Destination{URL: "https://dest.example.test/"})
	// 1 MiB + 1 byte — over the receiver's MaxInboundBody.
	big := bytes.Repeat([]byte{'x'}, webhookrelay.MaxInboundBody+1)
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/"+token,
		bytes.NewReader(big))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRelayReceiver_FreeUserEnforceReturns403(t *testing.T) {
	env := newRelayEnv(t, true) // enforce on
	token, _ := env.seedRelay(t, webhookrelay.Destination{URL: "https://dest.example.test/"})
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/"+token,
		bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Pro") {
		t.Errorf("body should mention Pro; got %s", rr.Body.String())
	}
}

func TestRelayReceiver_FreeUserReportOnlyEmitsLogAndAccepts(t *testing.T) {
	env := newRelayEnv(t, false) // enforce off → report-only
	token, _ := env.seedRelay(t, webhookrelay.Destination{URL: "https://dest.example.test/"})
	req := httptest.NewRequest(http.MethodPost, "/webhook-relay/"+token,
		bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	env.router.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status: got %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	out := env.logBuf.String()
	for _, want := range []string{
		`"msg":"entitlements.report_only_deny"`,
		`"feature":"webhook_relay"`,
		`"mode":"report_only"`,
		`"surface":"webhook-relay"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n%s", want, out)
		}
	}
}

