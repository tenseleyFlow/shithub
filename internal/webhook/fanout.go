// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// FanoutConsumer is the consumer name in domain_events_processed.
// Distinct from notif's so cursors don't collide.
const FanoutConsumer = "webhook_fanout"

// FanoutBatch caps how many events a single tick drains.
const FanoutBatch = 200

// FanoutDeps wires the fan-out against the runtime.
type FanoutDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// FanoutOnce drains domain_events past the persisted cursor, finds
// matching webhooks, and inserts a webhook_deliveries row + enqueues a
// `webhook:deliver` job per match. Returns the number of events
// processed; the caller decides to re-enqueue when full.
//
// The consumer cursor is advanced after each event regardless of
// whether the event matched any subscribers — an event without
// subscribers is "processed" the same as one with five.
func FanoutOnce(ctx context.Context, deps FanoutDeps) (int, error) {
	if deps.Pool == nil {
		return 0, errors.New("webhook fanout: nil Pool")
	}
	q := webhookdb.New()
	cur, err := q.GetWebhookCursor(ctx, deps.Pool, FanoutConsumer)
	last := int64(0)
	if err == nil {
		last = cur.LastEventID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return 0, fmt.Errorf("webhook fanout: load cursor: %w", err)
	}
	rows, err := q.ListUnprocessedDomainEvents(ctx, deps.Pool, webhookdb.ListUnprocessedDomainEventsParams{
		ID:    last,
		Limit: FanoutBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("webhook fanout: list events: %w", err)
	}
	processed := 0
	for _, ev := range rows {
		if err := dispatchEvent(ctx, deps, q, ev); err != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "webhook fanout: dispatch failed",
					"event_id", ev.ID, "kind", ev.Kind, "error", err)
			}
			break
		}
		last = ev.ID
		processed++
	}
	if processed > 0 {
		if err := q.SetWebhookCursor(ctx, deps.Pool, webhookdb.SetWebhookCursorParams{
			Consumer: FanoutConsumer, LastEventID: last,
		}); err != nil {
			return processed, fmt.Errorf("webhook fanout: persist cursor: %w", err)
		}
	}
	return processed, nil
}

// dispatchEvent finds matching webhooks for one domain event, creates
// delivery rows, and enqueues per-row deliver jobs.
func dispatchEvent(ctx context.Context, deps FanoutDeps, q *webhookdb.Queries, ev webhookdb.DomainEvent) error {
	// Resolve the owner pool. Repo events may match repo-level
	// webhooks AND the org-level webhooks (when the repo is org-owned).
	// User-owned repos have no second bucket.
	subs := []webhookdb.Webhook{}
	if ev.RepoID.Valid {
		repoSubs, err := q.ListActiveWebhooksForOwner(ctx, deps.Pool, webhookdb.ListActiveWebhooksForOwnerParams{
			OwnerKind: webhookdb.WebhookOwnerKindRepo,
			OwnerID:   ev.RepoID.Int64,
		})
		if err != nil {
			return fmt.Errorf("list repo webhooks: %w", err)
		}
		subs = append(subs, repoSubs...)
		// Org-level: look up the repo's owner_org_id; only org-owned
		// repos pick up org-level webhooks.
		owner, err := q.GetRepoOwnerKindForFanout(ctx, deps.Pool, ev.RepoID.Int64)
		if err == nil && owner.OwnerOrgID.Valid {
			orgSubs, err := q.ListActiveWebhooksForOwner(ctx, deps.Pool, webhookdb.ListActiveWebhooksForOwnerParams{
				OwnerKind: webhookdb.WebhookOwnerKindOrg,
				OwnerID:   owner.OwnerOrgID.Int64,
			})
			if err != nil {
				return fmt.Errorf("list org webhooks: %w", err)
			}
			subs = append(subs, orgSubs...)
		}
	}
	if len(subs) == 0 {
		return nil
	}
	for _, w := range subs {
		if !subscribesToKind(w.Events, ev.Kind) {
			continue
		}
		body, headersJSON, err := buildPayload(ev, w)
		if err != nil {
			if deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "webhook fanout: build payload",
					"event_id", ev.ID, "webhook_id", w.ID, "error", err)
			}
			continue
		}
		idem := idempotencyKey(w.ID, ev.ID, body)
		row, err := q.CreateDelivery(ctx, deps.Pool, webhookdb.CreateDeliveryParams{
			WebhookID:      w.ID,
			EventKind:      ev.Kind,
			EventID:        pgtype.Int8{Int64: ev.ID, Valid: true},
			Payload:        body,
			RequestHeaders: headersJSON,
			RequestBody:    body,
			Attempt:        1,
			MaxAttempts:    8,
			NextRetryAt:    pgtype.Timestamptz{Time: time.Now(), Valid: true},
			Status:         webhookdb.WebhookDeliveryStatusPending,
			IdempotencyKey: idem,
			RedeliverOf:    pgtype.Int8{Valid: false},
		})
		if err != nil {
			return fmt.Errorf("create delivery: %w", err)
		}
		if _, err := worker.Enqueue(ctx, deps.Pool, KindWebhookDeliver,
			deliverPayload{DeliveryID: row.ID}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
			deps.Logger.WarnContext(ctx, "webhook fanout: enqueue deliver",
				"delivery_id", row.ID, "error", err)
		}
	}
	return nil
}

// subscribesToKind returns true when the webhook's `events` filter
// either is empty (= "all") or contains the event kind.
func subscribesToKind(events []string, kind string) bool {
	if len(events) == 0 {
		return true
	}
	for _, e := range events {
		if e == kind || e == "*" {
			return true
		}
	}
	return false
}

// buildPayload assembles the JSON body for delivery. We pass the
// domain_events row through verbatim under `event` plus a tiny
// envelope; subscribers care about both the kind and the payload.
func buildPayload(ev webhookdb.DomainEvent, w webhookdb.Webhook) (body []byte, headers []byte, err error) {
	envelope := map[string]any{
		"event_id":   ev.ID,
		"event_kind": ev.Kind,
		"created_at": ev.CreatedAt.Time.UTC().Format(time.RFC3339Nano),
		"webhook_id": w.ID,
		"payload":    json.RawMessage(ev.Payload),
	}
	if ev.RepoID.Valid {
		envelope["repo_id"] = ev.RepoID.Int64
	}
	if ev.ActorUserID.Valid {
		envelope["actor_user_id"] = ev.ActorUserID.Int64
	}
	body, err = json.Marshal(envelope)
	if err != nil {
		return nil, nil, err
	}
	hdrs := map[string]any{
		"User-Agent":      "shithub-Hookshot",
		"Content-Type":    contentTypeHeader(w.ContentType),
		"X-Shithub-Event": ev.Kind,
		"X-Shithub-Hook-Installation-Target-Type": string(w.OwnerKind),
		"X-Shithub-Hook-Installation-Target-Id":   strconv.FormatInt(w.OwnerID, 10),
	}
	headers, err = json.Marshal(hdrs)
	if err != nil {
		return nil, nil, err
	}
	return body, headers, nil
}

// contentTypeHeader maps the enum to its on-the-wire MIME.
func contentTypeHeader(ct webhookdb.WebhookContentType) string {
	switch ct {
	case webhookdb.WebhookContentTypeForm:
		return "application/x-www-form-urlencoded"
	default:
		return "application/json"
	}
}

// idempotencyKey is sha256(payload || webhook_id || event_id). Stable
// across retries so subscribers can dedupe.
func idempotencyKey(webhookID, eventID int64, body []byte) string {
	h := sha256.New()
	h.Write(body)
	h.Write([]byte(strconv.FormatInt(webhookID, 10)))
	h.Write([]byte(strconv.FormatInt(eventID, 10)))
	return hex.EncodeToString(h.Sum(nil))
}
