// SPDX-License-Identifier: AGPL-3.0-or-later

package webhookrelay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	relaydb "github.com/tenseleyFlow/shithub/internal/webhookrelay/sqlc"
)

// DefaultMaxAttempts mirrors the repo-webhook subsystem so operators
// only have to memorize one retry budget across the two surfaces.
const DefaultMaxAttempts = 8

// MaxInboundBody bounds the inbound payload size at the receiver.
// 1 MiB is the relay-layer cap (vs 25 MiB at the repo-webhook layer)
// because relays carry control-plane events, not blob shipping. A
// raise would also bloat payload_bytes storage for retries.
const MaxInboundBody = 1 * 1024 * 1024

// IngestResult bundles the request id assigned to this inbound POST
// (returned to the caller via X-Request-ID for traceability) and the
// number of delivery rows created.
//
// DestinationsAttempted is the count of destinations the receiver
// tried to fan out to; the handler uses it to distinguish "zero
// destinations configured (user choice — 202)" from "destinations
// configured but every CreateDelivery failed (broken — 503)" so a
// broken relay doesn't silently swallow inbound posts.
// PRO-EXT_SR2-13 (audit Q5).
type IngestResult struct {
	RequestID             string
	DeliveryRows          int
	DestinationsAttempted int
}

// Ingest takes a relay row + raw inbound body and creates one
// pending delivery row per configured destination, then enqueues a
// worker job per row.
//
// A relay with zero destinations is a no-op (no rows, no enqueues)
// but the receiver still returns success — the user configured the
// upstream, they just haven't wired any downstream targets yet.
func (d Deps) Ingest(ctx context.Context, logger *slog.Logger, r Relay, body []byte) (IngestResult, error) {
	if r.Disabled {
		return IngestResult{}, ErrDisabled
	}
	dests := r.Destinations
	if len(dests) > MaxDestinations {
		// Defense in depth: PR 13c's create handler caps, but a row
		// could have been seeded out-of-band. Truncate rather than
		// reject — the receiver isn't where we tell the user about
		// a mis-shaped relay; the settings page is.
		dests = dests[:MaxDestinations]
	}
	reqID, err := newRequestID()
	if err != nil {
		return IngestResult{}, fmt.Errorf("webhookrelay: gen request id: %w", err)
	}
	res := IngestResult{RequestID: reqID, DestinationsAttempted: len(dests)}
	if len(dests) == 0 {
		return res, nil
	}
	q := relaydb.New()
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	for _, dest := range dests {
		row, err := q.CreateDelivery(ctx, d.Pool, relaydb.CreateDeliveryParams{
			RelayID:        r.ID,
			DestinationUrl: dest.URL,
			Status:         relaydb.WebhookRelayDeliveryStatusPending,
			Attempt:        0,
			MaxAttempts:    DefaultMaxAttempts,
			NextAttemptAt:  now,
			PayloadBytes:   body,
			RequestID:      reqID,
		})
		if err != nil {
			// Logged but not fatal: the rest of the fan-out should
			// still go through. If every destination fails, the
			// caller sees zero deliveries created and can log that
			// at their level.
			if logger != nil {
				logger.WarnContext(ctx, "webhookrelay: create delivery row failed",
					"relay_id", r.ID, "destination", dest.URL,
					"error", err)
			}
			continue
		}
		if _, err := enqueueDeliver(ctx, d.Pool, row.ID); err != nil {
			// Same here — log and move on. The row exists; a
			// follow-up cron sweep (added in 13c) can pick up
			// orphaned pending rows whose enqueue dropped.
			if logger != nil {
				logger.WarnContext(ctx, "webhookrelay: enqueue deliver failed",
					"delivery_id", row.ID, "error", err)
			}
		}
		res.DeliveryRows++
	}
	return res, nil
}

// newRequestID returns a 16-byte hex string used as the
// X-Shithub-Relay-Request header on outbound deliveries. Independent
// of the worker job id so a caller (or destination) can correlate the
// inbound POST with the N outbound attempts.
func newRequestID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
