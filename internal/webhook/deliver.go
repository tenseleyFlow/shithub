// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
)

// ResponseBodyCap bounds the response body we keep in DB so a malicious
// receiver can't blow up our storage. Above this we set
// response_truncated=true.
const ResponseBodyCap = 32 * 1024

// MaxBodySize caps the request body at 25 MiB per the spec. We don't
// build payloads anywhere near this, but the cap protects against
// pathological event shapes.
const MaxBodySize = 25 * 1024 * 1024

// DeliverDeps wires the deliverer.
type DeliverDeps struct {
	Pool       *pgxpool.Pool
	Logger     *slog.Logger
	HTTPClient *http.Client
	// SecretBox is the primary AEAD box used to decrypt webhook
	// secrets (and encrypt new ones via ManageDeps). When
	// LegacySecretBox is also set, OpenSecret tries the primary
	// first and falls back to legacy for rows that haven't been
	// re-encrypted yet — see internal/webhook/secrets.go.
	SecretBox       *secretbox.Box
	LegacySecretBox *secretbox.Box
	SSRF            SSRFConfig
	// JitterFn is plumbed for tests; nil => math/rand.Float64.
	JitterFn func() float64
}

// Deliver handles one delivery row end-to-end: load + decrypt secret,
// validate URL via SSRF defense, POST, record outcome, schedule retry
// or mark terminal. Returns nil on success, an error to be logged on
// transient failure (the row is updated regardless).
func Deliver(ctx context.Context, deps DeliverDeps, deliveryID int64) error {
	if deps.Pool == nil {
		return errors.New("webhook deliver: nil Pool")
	}
	if deps.SecretBox == nil {
		return errors.New("webhook deliver: nil SecretBox")
	}
	q := webhookdb.New()
	row, err := q.GetDeliveryByID(ctx, deps.Pool, deliveryID)
	if err != nil {
		return fmt.Errorf("load delivery: %w", err)
	}
	// Already-terminal rows can land here when a redelivery is
	// re-enqueued; nothing to do.
	if row.Status == webhookdb.WebhookDeliveryStatusSucceeded ||
		row.Status == webhookdb.WebhookDeliveryStatusFailedPermanent {
		return nil
	}
	hook, err := q.GetWebhookByID(ctx, deps.Pool, row.WebhookID)
	if err != nil {
		return fmt.Errorf("load webhook: %w", err)
	}
	if !hook.Active || hook.DisabledAt.Valid {
		// Owner disabled the hook between fanout and delivery — mark
		// the delivery permanently failed so it doesn't loop.
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, webhookdb.MarkDeliveryPermanentFailureParams{
			ID:                deliveryID,
			ResponseStatus:    pgtype.Int4{Valid: false},
			ResponseHeaders:   nil,
			ResponseBody:      nil,
			ResponseTruncated: false,
			ErrorSummary:      "webhook disabled before delivery",
		})
		return nil
	}
	secret, err := OpenSecret(deps.SecretBox, deps.LegacySecretBox, hook.SecretCiphertext, hook.SecretNonce)
	if err != nil {
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, webhookdb.MarkDeliveryPermanentFailureParams{
			ID:                deliveryID,
			ResponseStatus:    pgtype.Int4{Valid: false},
			ResponseHeaders:   nil,
			ResponseBody:      nil,
			ResponseTruncated: false,
			ErrorSummary:      "decrypt secret: " + err.Error(),
		})
		_ = q.AutoDisableWebhook(ctx, deps.Pool, webhookdb.AutoDisableWebhookParams{
			ID:             hook.ID,
			DisabledReason: pgtype.Text{String: "secret decryption failure", Valid: true},
		})
		return nil
	}

	if err := deps.SSRF.Validate(hook.Url); err != nil {
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, webhookdb.MarkDeliveryPermanentFailureParams{
			ID:                deliveryID,
			ResponseStatus:    pgtype.Int4{Valid: false},
			ResponseHeaders:   nil,
			ResponseBody:      nil,
			ResponseTruncated: false,
			ErrorSummary:      "ssrf: " + err.Error(),
		})
		recordFailure(ctx, q, deps, hook)
		return nil
	}

	if len(row.RequestBody) > MaxBodySize {
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, webhookdb.MarkDeliveryPermanentFailureParams{
			ID:                deliveryID,
			ResponseStatus:    pgtype.Int4{Valid: false},
			ResponseHeaders:   nil,
			ResponseBody:      nil,
			ResponseTruncated: false,
			ErrorSummary:      fmt.Sprintf("payload exceeds %d bytes", MaxBodySize),
		})
		return nil
	}

	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = deps.SSRF.HTTPClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook.Url, bytes.NewReader(row.RequestBody))
	if err != nil {
		return scheduleRetryOrPermanent(ctx, q, deps, row, hook, nil, "build request: "+err.Error())
	}
	applyHeaders(req, row.RequestHeaders, row.DeliveryUuid.String(), secret)

	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		// Connection errors are retryable.
		return scheduleRetryOrPermanent(ctx, q, deps, row, hook, nil, "transport: "+doErr.Error())
	}
	defer resp.Body.Close()

	bodyBytes, truncated, _ := readCappedBody(resp.Body, ResponseBodyCap)
	respHeaders, _ := json.Marshal(headerMap(resp.Header))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		// 3xx is treated as success per spec — we don't follow
		// redirects (the SSRF check covered the original URL only).
		_ = q.MarkDeliverySucceeded(ctx, deps.Pool, webhookdb.MarkDeliverySucceededParams{
			ID:                deliveryID,
			ResponseStatus:    pgtype.Int4{Int32: int32(resp.StatusCode), Valid: true},
			ResponseHeaders:   respHeaders,
			ResponseBody:      bodyBytes,
			ResponseTruncated: truncated,
		})
		_ = q.RecordWebhookSuccess(ctx, deps.Pool, hook.ID)
		return nil
	case isRetryableStatus(resp.StatusCode):
		return scheduleRetryOrPermanentWithResp(ctx, q, deps, row, hook,
			resp.StatusCode, respHeaders, bodyBytes, truncated,
			fmt.Sprintf("HTTP %d (retryable)", resp.StatusCode))
	default:
		// Non-retryable 4xx (other than 408/429): terminal failure.
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, webhookdb.MarkDeliveryPermanentFailureParams{
			ID:                deliveryID,
			ResponseStatus:    pgtype.Int4{Int32: int32(resp.StatusCode), Valid: true},
			ResponseHeaders:   respHeaders,
			ResponseBody:      bodyBytes,
			ResponseTruncated: truncated,
			ErrorSummary:      fmt.Sprintf("HTTP %d (non-retryable)", resp.StatusCode),
		})
		recordFailure(ctx, q, deps, hook)
		return nil
	}
}

// scheduleRetryOrPermanent decides whether `row` should be retried
// (next_retry_at = now + backoff(attempt+1)) or marked terminal
// (attempt+1 > max_attempts). Used for transport-level failures with
// no HTTP response.
func scheduleRetryOrPermanent(ctx context.Context, q *webhookdb.Queries, deps DeliverDeps, row webhookdb.WebhookDelivery, hook webhookdb.Webhook, respHeaders []byte, summary string) error {
	return scheduleRetryOrPermanentWithResp(ctx, q, deps, row, hook, 0, respHeaders, nil, false, summary)
}

func scheduleRetryOrPermanentWithResp(ctx context.Context, q *webhookdb.Queries, deps DeliverDeps, row webhookdb.WebhookDelivery, hook webhookdb.Webhook, status int, respHeaders []byte, body []byte, truncated bool, summary string) error {
	nextAttempt := row.Attempt + 1
	jitter := deps.JitterFn
	if jitter == nil {
		jitter = rand.Float64
	}
	if int(nextAttempt) > int(row.MaxAttempts) {
		statusVal := pgtype.Int4{Valid: false}
		if status > 0 {
			statusVal = pgtype.Int4{Int32: int32(status), Valid: true}
		}
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, webhookdb.MarkDeliveryPermanentFailureParams{
			ID:                row.ID,
			ResponseStatus:    statusVal,
			ResponseHeaders:   respHeaders,
			ResponseBody:      body,
			ResponseTruncated: truncated,
			ErrorSummary:      summary + " (max_attempts exhausted)",
		})
		recordFailure(ctx, q, deps, hook)
		return nil
	}
	delay := Backoff(int(nextAttempt), jitter)
	statusVal := pgtype.Int4{Valid: false}
	if status > 0 {
		statusVal = pgtype.Int4{Int32: int32(status), Valid: true}
	}
	_ = q.MarkDeliveryRetry(ctx, deps.Pool, webhookdb.MarkDeliveryRetryParams{
		ID:                row.ID,
		ResponseStatus:    statusVal,
		ResponseHeaders:   respHeaders,
		ResponseBody:      body,
		ResponseTruncated: truncated,
		NextRetryAt:       pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true},
		ErrorSummary:      summary,
	})
	return nil
}

// recordFailure increments consecutive_failures and auto-disables when
// the threshold is crossed.
func recordFailure(ctx context.Context, q *webhookdb.Queries, deps DeliverDeps, hook webhookdb.Webhook) {
	row, err := q.RecordWebhookFailure(ctx, deps.Pool, hook.ID)
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "webhook deliver: record failure", "error", err)
		}
		return
	}
	if int(row.ConsecutiveFailures) >= int(row.AutoDisableThreshold) {
		_ = q.AutoDisableWebhook(ctx, deps.Pool, webhookdb.AutoDisableWebhookParams{
			ID:             hook.ID,
			DisabledReason: pgtype.Text{String: fmt.Sprintf("auto-disabled after %d consecutive failures", row.ConsecutiveFailures), Valid: true},
		})
	}
}

// applyHeaders writes the per-delivery headers + signature onto the
// outbound request. The HMAC is computed over the raw body so a
// receiver that re-serializes JSON will fail verification — that's the
// signal we want.
func applyHeaders(req *http.Request, headersJSON []byte, deliveryUUID, secret string) {
	if len(headersJSON) > 0 {
		var m map[string]string
		if err := json.Unmarshal(headersJSON, &m); err == nil {
			for k, v := range m {
				req.Header.Set(k, v)
			}
		}
	}
	body, _ := io.ReadAll(req.Body)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("X-Shithub-Delivery", deliveryUUID)
	req.Header.Set("X-Shithub-Signature-256", SignSHA256([]byte(secret), body))
}

// isRetryableStatus reports whether the deliverer should reschedule on
// this HTTP status. Per spec: 408 (Request Timeout), 429 (Too Many
// Requests), and any 5xx.
func isRetryableStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status < 600
}

// readCappedBody reads up to cap+1 bytes; if it overshoots, the extra
// is discarded and `truncated` is true.
func readCappedBody(r io.Reader, cap int) (body []byte, truncated bool, err error) {
	limited := io.LimitReader(r, int64(cap)+1)
	body, err = io.ReadAll(limited)
	if err != nil {
		return body, false, err
	}
	if len(body) > cap {
		body = body[:cap]
		truncated = true
		// Drain the rest so the connection can be reused (we have
		// keep-alives off, but politeness for any future change).
		_, _ = io.Copy(io.Discard, r)
	}
	return body, truncated, nil
}

// headerMap turns http.Header into the {name: value} map we store. We
// take only the first value per key — webhooks are POSTs to JSON
// endpoints, multi-value headers are vanishingly rare.
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// EnqueuePing creates a synthetic ping delivery and enqueues it. Used
// on webhook create / re-enable so the operator sees an immediate
// round-trip.
func EnqueuePing(ctx context.Context, deps FanoutDeps, hookID int64) error {
	q := webhookdb.New()
	hook, err := q.GetWebhookByID(ctx, deps.Pool, hookID)
	if err != nil {
		return fmt.Errorf("load webhook: %w", err)
	}
	body, _ := json.Marshal(map[string]any{
		"zen":  "Approachable is better than simple.",
		"hook": map[string]any{"id": hook.ID, "url": hook.Url},
	})
	hdrs, _ := json.Marshal(map[string]string{
		"User-Agent":      "shithub-Hookshot",
		"Content-Type":    contentTypeHeader(hook.ContentType),
		"X-Shithub-Event": "ping",
	})
	row, err := q.CreateDelivery(ctx, deps.Pool, webhookdb.CreateDeliveryParams{
		WebhookID:      hookID,
		EventKind:      "ping",
		EventID:        pgtype.Int8{Valid: false},
		Payload:        body,
		RequestHeaders: hdrs,
		RequestBody:    body,
		Attempt:        1,
		MaxAttempts:    3,
		NextRetryAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:         webhookdb.WebhookDeliveryStatusPending,
		IdempotencyKey: idempotencyKey(hookID, 0, body),
		RedeliverOf:    pgtype.Int8{Valid: false},
	})
	if err != nil {
		return fmt.Errorf("create ping delivery: %w", err)
	}
	if _, err := workerEnqueueDeliver(ctx, deps.Pool, row.ID); err != nil {
		return fmt.Errorf("enqueue ping: %w", err)
	}
	return nil
}

// Redeliver clones a past delivery into a new pending row so the UI's
// "redeliver" button preserves the audit trail (redeliver_of points
// at the original).
func Redeliver(ctx context.Context, deps FanoutDeps, originalID int64) (int64, error) {
	q := webhookdb.New()
	orig, err := q.GetDeliveryByID(ctx, deps.Pool, originalID)
	if err != nil {
		return 0, fmt.Errorf("load original: %w", err)
	}
	row, err := q.CreateDelivery(ctx, deps.Pool, webhookdb.CreateDeliveryParams{
		WebhookID:      orig.WebhookID,
		EventKind:      orig.EventKind,
		EventID:        orig.EventID,
		Payload:        orig.Payload,
		RequestHeaders: orig.RequestHeaders,
		RequestBody:    orig.RequestBody,
		Attempt:        1,
		MaxAttempts:    orig.MaxAttempts,
		NextRetryAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
		Status:         webhookdb.WebhookDeliveryStatusPending,
		IdempotencyKey: orig.IdempotencyKey,
		RedeliverOf:    pgtype.Int8{Int64: orig.ID, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("create redelivery: %w", err)
	}
	if _, err := workerEnqueueDeliver(ctx, deps.Pool, row.ID); err != nil {
		return row.ID, fmt.Errorf("enqueue redelivery: %w", err)
	}
	return row.ID, nil
}
