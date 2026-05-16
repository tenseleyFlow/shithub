// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/worker"
)

// KindWebhookRelayDeliver is the worker job kind for outbound relay
// delivery attempts. One job per pending delivery row; the deliver
// path either marks the row terminal or schedules a follow-up via
// next_attempt_at + a fresh enqueue.
const KindWebhookRelayDeliver worker.Kind = "webhook_relay:deliver"

// deliverPayload is the schema for a webhook_relay:deliver job.
type deliverPayload struct {
	DeliveryID int64 `json:"delivery_id"`
}

// MarshalDeliverPayload exposes the json shape so the worker
// registration and tests build the same payload.
func MarshalDeliverPayload(deliveryID int64) ([]byte, error) {
	return json.Marshal(deliverPayload{DeliveryID: deliveryID})
}

// UnmarshalDeliverPayload reads the id from a worker job's raw JSON.
func UnmarshalDeliverPayload(raw json.RawMessage) (int64, error) {
	var p deliverPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	return p.DeliveryID, nil
}

// enqueueDeliver writes the worker job row for a freshly-created
// delivery. Tests can call this directly; production goes through
// Ingest.
func enqueueDeliver(ctx context.Context, pool *pgxpool.Pool, deliveryID int64) (int64, error) {
	id, err := worker.Enqueue(ctx, pool, KindWebhookRelayDeliver,
		deliverPayload{DeliveryID: deliveryID}, worker.EnqueueOptions{})
	if err != nil {
		return 0, fmt.Errorf("webhookrelay: enqueue deliver: %w", err)
	}
	return id, nil
}
