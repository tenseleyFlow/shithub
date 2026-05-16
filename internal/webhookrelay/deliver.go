// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	relaydb "github.com/tenseleyFlow/shithub/internal/webhookrelay/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// HeaderSignature is the outbound HMAC header. Same shape as
// X-Shithub-Signature-256 so users wiring up downstream receivers
// can paste the same verification snippet that works for repo
// webhooks.
const HeaderSignature = "X-Shithub-Relay-Signature-256"

// HeaderRequestID lets the destination correlate one outbound POST
// with the inbound POST it was fanned out from. Same id on every
// destination's delivery.
const HeaderRequestID = "X-Shithub-Relay-Request"

// HeaderDeliveryID lets the destination distinguish retries from
// fresh attempts. Distinct per delivery row.
const HeaderDeliveryID = "X-Shithub-Relay-Delivery"

// DeliverDeps wires the worker handler. JitterFn is nil-friendly for
// tests; production passes rand.Float64.
type DeliverDeps struct {
	Pool       *pgxpool.Pool
	Logger     *slog.Logger
	SecretBox  *secretbox.Box
	SSRF       webhook.SSRFConfig
	HTTPClient *http.Client
	JitterFn   func() float64
}

// Deliver handles one delivery row end-to-end: load the relay (to
// fetch the HMAC secret, validate not-disabled, re-check SSRF), POST,
// record outcome, schedule retry or mark terminal.
//
// The mid-flight disable check is important: a user who notices a
// relay being abused can disable it from settings, and pending
// deliveries should stop draining within one tick rather than wait
// for the queue to empty.
func Deliver(ctx context.Context, deps DeliverDeps, deliveryID int64) error {
	if deps.Pool == nil {
		return errors.New("webhookrelay deliver: nil Pool")
	}
	if deps.SecretBox == nil {
		return errors.New("webhookrelay deliver: nil SecretBox")
	}
	q := relaydb.New()
	row, err := q.GetDeliveryByID(ctx, deps.Pool, deliveryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row was deleted (relay cascade) — nothing to do.
			return nil
		}
		return fmt.Errorf("load delivery: %w", err)
	}
	if row.Status == relaydb.WebhookRelayDeliveryStatusSucceeded ||
		row.Status == relaydb.WebhookRelayDeliveryStatusFailedPermanent {
		// Idempotency: another worker already finalized this row.
		return nil
	}

	relay, hmacSecret, err := Deps{Pool: deps.Pool, Box: deps.SecretBox}.GetByID(ctx, row.RelayID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Relay deleted between create and dispatch — mark
			// permanently failed so the row doesn't loop.
			_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, relaydb.MarkDeliveryPermanentFailureParams{
				ID:             deliveryID,
				LastStatusCode: pgtype.Int4{Valid: false},
				LastError:      pgtype.Text{String: "relay deleted", Valid: true},
			})
			return nil
		}
		return fmt.Errorf("load relay: %w", err)
	}
	if relay.Disabled {
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, relaydb.MarkDeliveryPermanentFailureParams{
			ID:             deliveryID,
			LastStatusCode: pgtype.Int4{Valid: false},
			LastError:      pgtype.Text{String: "relay disabled before delivery", Valid: true},
		})
		return nil
	}

	// Validate URL each attempt (DNS rebind defense).
	if err := deps.SSRF.Validate(row.DestinationUrl); err != nil {
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, relaydb.MarkDeliveryPermanentFailureParams{
			ID:             deliveryID,
			LastStatusCode: pgtype.Int4{Valid: false},
			LastError:      pgtype.Text{String: "ssrf: " + err.Error(), Valid: true},
		})
		return nil
	}

	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = deps.SSRF.HTTPClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		row.DestinationUrl, bytes.NewReader(row.PayloadBytes))
	if err != nil {
		return scheduleRetryOrPermanent(ctx, q, deps, row, 0,
			"build request: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "shithub-relay")
	req.Header.Set(HeaderRequestID, row.RequestID)
	req.Header.Set(HeaderDeliveryID, fmt.Sprintf("%d", deliveryID))
	req.Header.Set(HeaderSignature, webhook.SignSHA256(hmacSecret, row.PayloadBytes))

	resp, doErr := httpClient.Do(req)
	if doErr != nil {
		// Transport errors are retryable.
		return scheduleRetryOrPermanent(ctx, q, deps, row, 0,
			"transport: "+doErr.Error())
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 400:
		_ = q.MarkDeliverySucceeded(ctx, deps.Pool, relaydb.MarkDeliverySucceededParams{
			ID:             deliveryID,
			LastStatusCode: pgtype.Int4{Int32: int32(resp.StatusCode), Valid: true},
		})
		return nil
	case isRetryableStatus(resp.StatusCode):
		return scheduleRetryOrPermanent(ctx, q, deps, row, resp.StatusCode,
			fmt.Sprintf("HTTP %d (retryable)", resp.StatusCode))
	default:
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, relaydb.MarkDeliveryPermanentFailureParams{
			ID:             deliveryID,
			LastStatusCode: pgtype.Int4{Int32: int32(resp.StatusCode), Valid: true},
			LastError:      pgtype.Text{String: fmt.Sprintf("HTTP %d (non-retryable)", resp.StatusCode), Valid: true},
		})
		return nil
	}
}

// scheduleRetryOrPermanent picks between failed_retry (with
// next_attempt_at) and failed_permanent (max_attempts exhausted).
func scheduleRetryOrPermanent(ctx context.Context, q *relaydb.Queries, deps DeliverDeps,
	row relaydb.WebhookRelayDelivery, status int, summary string,
) error {
	nextAttempt := row.Attempt + 1
	statusVal := pgtype.Int4{Valid: false}
	if status > 0 {
		statusVal = pgtype.Int4{Int32: int32(status), Valid: true}
	}
	if int(nextAttempt) > int(row.MaxAttempts) {
		_ = q.MarkDeliveryPermanentFailure(ctx, deps.Pool, relaydb.MarkDeliveryPermanentFailureParams{
			ID:             row.ID,
			LastStatusCode: statusVal,
			LastError:      pgtype.Text{String: summary + " (max_attempts exhausted)", Valid: true},
		})
		return nil
	}
	jitter := deps.JitterFn
	if jitter == nil {
		jitter = rand.Float64
	}
	delay := webhook.Backoff(int(nextAttempt), jitter)
	_ = q.MarkDeliveryRetry(ctx, deps.Pool, relaydb.MarkDeliveryRetryParams{
		ID:             row.ID,
		NextAttemptAt:  pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true},
		LastStatusCode: statusVal,
		LastError:      pgtype.Text{String: summary, Valid: true},
	})
	// Enqueue a follow-up job; the dispatcher will pick it up at
	// next_attempt_at via the job-table run_at field.
	if _, err := worker.Enqueue(ctx, deps.Pool, KindWebhookRelayDeliver,
		deliverPayload{DeliveryID: row.ID},
		worker.EnqueueOptions{
			RunAt: pgtype.Timestamptz{Time: time.Now().Add(delay), Valid: true},
		}); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "webhookrelay: re-enqueue retry failed",
			"delivery_id", row.ID, "error", err)
	}
	return nil
}

// isRetryableStatus mirrors the repo-webhook policy: 408/429 + 5xx.
func isRetryableStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status < 600
}
