// SPDX-License-Identifier: AGPL-3.0-or-later

package webhook

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/worker"
)

// Job kinds shipped by S33. Registered by cmd/shithubd/worker.go.
const (
	KindWebhookFanout    worker.Kind = "webhook:fanout"
	KindWebhookDeliver   worker.Kind = "webhook:deliver"
	KindWebhookPurgeOld  worker.Kind = "webhook:purge_old"
)

// deliverPayload is the schema for a `webhook:deliver` job. The
// fanout creates one job per delivery row; redeliveries enqueue
// directly through this same shape.
type deliverPayload struct {
	DeliveryID int64 `json:"delivery_id"`
}

// MarshalDeliverPayload exposes the json shape so downstream packages
// (worker registration, tests) build the same payload without
// duplicating the struct.
func MarshalDeliverPayload(deliveryID int64) ([]byte, error) {
	return json.Marshal(deliverPayload{DeliveryID: deliveryID})
}

// UnmarshalDeliverPayload extracts the id from a worker job's raw
// JSON. Used by the registered handler.
func UnmarshalDeliverPayload(raw json.RawMessage) (int64, error) {
	var p deliverPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	return p.DeliveryID, nil
}

// workerEnqueueDeliver is the convenience the in-package callers
// (ping, redeliver) use so they don't have to repeat the marshal +
// enqueue boilerplate.
func workerEnqueueDeliver(ctx context.Context, pool *pgxpool.Pool, deliveryID int64) (int64, error) {
	return worker.Enqueue(ctx, pool, KindWebhookDeliver,
		deliverPayload{DeliveryID: deliveryID}, worker.EnqueueOptions{})
}
