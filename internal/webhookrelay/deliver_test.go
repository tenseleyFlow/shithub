// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tenseleyFlow/shithub/internal/security/ssrf"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
	relaydb "github.com/tenseleyFlow/shithub/internal/webhookrelay/sqlc"
)

// loopbackSSRF returns an SSRF config that allows 127.0.0.1 + any
// port so httptest.Server endpoints (random high port) round-trip.
// Production never sets this — DefaultSSRFConfig keeps the strict
// port whitelist.
func loopbackSSRF() webhook.SSRFConfig {
	ports := make([]int, 65535)
	for i := range ports {
		ports[i] = i + 1
	}
	return ssrf.Config{
		AllowedSchemes:       []string{"http", "https"},
		AllowedPorts:         ports,
		AllowPrivateNetworks: true,
	}
}

func TestDeliver_HappyPathSignsAndMarksSucceeded(t *testing.T) {
	t.Parallel()
	f := setup(t)
	var receivedBody []byte
	var receivedSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedBody, _ = io.ReadAll(r.Body)
		receivedSig = r.Header.Get(webhookrelay.HeaderSignature)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	hmacSecret := []byte("the-shared-secret")
	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: hmacSecret,
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	payload := []byte(`{"hello":"world"}`)
	ingest, err := f.deps.Ingest(context.Background(), logger, res.Relay, payload)
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if ingest.DeliveryRows != 1 {
		t.Fatalf("DeliveryRows: got %d, want 1", ingest.DeliveryRows)
	}

	// Discover the delivery row id then dispatch.
	var deliveryID int64
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID); err != nil {
		t.Fatalf("find delivery id: %v", err)
	}

	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool:      f.pool,
		Logger:    logger,
		SecretBox: f.deps.Box,
		SSRF:      loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	// Body matched what we sent.
	if string(receivedBody) != string(payload) {
		t.Errorf("body: got %q, want %q", receivedBody, payload)
	}
	// Signature matches sha256 HMAC of the body with our secret.
	expectedSig := signSHA256(hmacSecret, payload)
	if receivedSig != expectedSig {
		t.Errorf("signature: got %q, want %q", receivedSig, expectedSig)
	}

	// DB row was marked succeeded.
	var status string
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT status::text FROM webhook_relay_deliveries WHERE id = $1`, deliveryID,
	).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != string(relaydb.WebhookRelayDeliveryStatusSucceeded) {
		t.Errorf("status: got %q, want succeeded", status)
	}
}

func signSHA256(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestDeliver_503MarksRetryAndReenqueues(t *testing.T) {
	t.Parallel()
	f := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	res, err := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)

	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	var status string
	var attempt int
	var lastCode int
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT status::text, attempt, last_status_code FROM webhook_relay_deliveries WHERE id = $1`,
		deliveryID,
	).Scan(&status, &attempt, &lastCode); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != string(relaydb.WebhookRelayDeliveryStatusFailedRetry) {
		t.Errorf("status: got %q, want failed_retry", status)
	}
	if attempt != 1 {
		t.Errorf("attempt: got %d, want 1", attempt)
	}
	if lastCode != 503 {
		t.Errorf("last_status_code: got %d, want 503", lastCode)
	}
	// Re-enqueue: there should now be 2 jobs (the initial one from
	// Ingest + the retry from Deliver).
	var jobs int
	if err := f.pool.QueryRow(
		context.Background(),
		`SELECT count(*) FROM jobs WHERE kind = 'webhook_relay:deliver'`,
	).Scan(&jobs); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobs != 2 {
		t.Errorf("worker jobs: got %d, want 2 (original + retry)", jobs)
	}
}

func TestDeliver_404MarksPermanent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	res, _ := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)

	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	var status string
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT status::text FROM webhook_relay_deliveries WHERE id = $1`,
		deliveryID,
	).Scan(&status)
	if status != string(relaydb.WebhookRelayDeliveryStatusFailedPermanent) {
		t.Errorf("status: got %q, want failed_permanent", status)
	}
}

func TestDeliver_MaxAttemptsMarksPermanent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	res, _ := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)
	// Bump attempt to one shy of max so the next deliver crosses
	// the boundary into failed_permanent.
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE webhook_relay_deliveries SET attempt = max_attempts WHERE id = $1`,
		deliveryID,
	); err != nil {
		t.Fatalf("bump attempt: %v", err)
	}

	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var status string
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT status::text FROM webhook_relay_deliveries WHERE id = $1`,
		deliveryID,
	).Scan(&status)
	if status != string(relaydb.WebhookRelayDeliveryStatusFailedPermanent) {
		t.Errorf("status: got %q, want failed_permanent", status)
	}
}

func TestDeliver_SSRFLoopbackBlockedUnderDefaultPolicy(t *testing.T) {
	t.Parallel()
	f := setup(t)
	// Don't start a real server — the SSRF policy should refuse
	// to dial 127.0.0.1 under DefaultSSRFConfig anyway. We use a
	// placeholder URL.
	res, _ := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: "http://127.0.0.1:9/"}},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)

	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: webhook.DefaultSSRFConfig(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	var status string
	var lastErr string
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT status::text, last_error FROM webhook_relay_deliveries WHERE id = $1`,
		deliveryID,
	).Scan(&status, &lastErr)
	if status != string(relaydb.WebhookRelayDeliveryStatusFailedPermanent) {
		t.Errorf("status: got %q, want failed_permanent (SSRF block)", status)
	}
	if !strings.Contains(lastErr, "ssrf") {
		t.Errorf("last_error: got %q, want ssrf prefix", lastErr)
	}
}

// TestDeliver_IdempotentOnTerminalRow pins that a re-dispatch of a
// row that's already terminal (succeeded or failed_permanent) is a
// no-op — important because retries can race with manual redeliver
// in the future, and we don't want to double-send.
func TestDeliver_IdempotentOnTerminalRow(t *testing.T) {
	t.Parallel()
	f := setup(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	res, _ := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)

	// First dispatch — should hit the server.
	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver 1: %v", err)
	}
	// Second dispatch — row is succeeded, must be a no-op.
	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver 2: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("server saw %d POSTs, want 1 (second call should no-op on terminal row)", got)
	}
}

// TestDeliver_DisabledRelayMarksPermanent pins that a relay disabled
// between Ingest and Deliver causes the row to terminate rather than
// loop forever waiting for re-enable.
func TestDeliver_DisabledRelayMarksPermanent(t *testing.T) {
	t.Parallel()
	f := setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	res, _ := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	_, _ = f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)

	// Disable the relay before dispatch.
	if err := f.deps.Disable(context.Background(), res.ID); err != nil {
		t.Fatalf("Disable: %v", err)
	}

	if err := webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box,
		SSRF: loopbackSSRF(),
	}, deliveryID); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var status string
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT status::text FROM webhook_relay_deliveries WHERE id = $1`,
		deliveryID,
	).Scan(&status)
	if status != string(relaydb.WebhookRelayDeliveryStatusFailedPermanent) {
		t.Errorf("status: got %q, want failed_permanent (disabled relay)", status)
	}
}

// TestDeliver_PayloadHeadersIncluded pins the contract on the
// outbound request headers — request id, delivery id, signature, and
// Content-Type. Used as a regression guard so a refactor to the
// header layer doesn't accidentally drop X-Shithub-Relay-Request,
// which destinations use to correlate the N fan-outs.
func TestDeliver_PayloadHeadersIncluded(t *testing.T) {
	t.Parallel()
	f := setup(t)
	var headers http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	res, _ := f.deps.Create(context.Background(), webhookrelay.CreateInput{
		UserID: f.userID, Name: "n", HMACSecret: []byte("k"),
		Destinations: []webhookrelay.Destination{{URL: srv.URL}},
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ingest, _ := f.deps.Ingest(context.Background(), logger, res.Relay, []byte(`{}`))
	var deliveryID int64
	_ = f.pool.QueryRow(
		context.Background(),
		`SELECT id FROM webhook_relay_deliveries WHERE relay_id = $1`, res.ID,
	).Scan(&deliveryID)

	_ = webhookrelay.Deliver(context.Background(), webhookrelay.DeliverDeps{
		Pool: f.pool, Logger: logger, SecretBox: f.deps.Box, SSRF: loopbackSSRF(),
	}, deliveryID)

	for _, h := range []string{
		webhookrelay.HeaderSignature,
		webhookrelay.HeaderRequestID,
		webhookrelay.HeaderDeliveryID,
		"Content-Type",
	} {
		if headers.Get(h) == "" {
			t.Errorf("missing header %q", h)
		}
	}
	if headers.Get(webhookrelay.HeaderRequestID) != ingest.RequestID {
		t.Errorf("request id mismatch: got %q, want %q",
			headers.Get(webhookrelay.HeaderRequestID), ingest.RequestID)
	}
	// Defensive — confirm Content-Type is the JSON value the
	// receiver promises.
	if !strings.HasPrefix(headers.Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type: got %q, want application/json...",
			headers.Get("Content-Type"))
	}
}

// Avoid unused-import on json in the unlikely case we delete every
// JSON-touching test — keeps the file robust to future trims.
var _ = json.Marshal
