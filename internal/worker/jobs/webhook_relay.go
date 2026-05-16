// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	"github.com/tenseleyFlow/shithub/internal/webhookrelay"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// WebhookRelayDeliverDeps wires the per-row deliverer. SecretBox
// decrypts the relay's HMAC secret (shared with the webhook
// subsystem's AEAD box — same SHITHUB_WEBHOOK__AEAD_KEY).
type WebhookRelayDeliverDeps struct {
	Pool       *pgxpool.Pool
	Logger     *slog.Logger
	SecretBox  *secretbox.Box
	SSRF       webhook.SSRFConfig
	HTTPClient *http.Client
}

// WebhookRelayDeliver dispatches one webhook-relay delivery row. The
// receiver (PRO-EXT01-13a) enqueues one job per pending row created
// during ingest; retries re-enqueue with a future RunAt via the
// scheduleRetryOrPermanent helper inside the package.
func WebhookRelayDeliver(deps WebhookRelayDeliverDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		id, err := webhookrelay.UnmarshalDeliverPayload(raw)
		if err != nil {
			return worker.PoisonError(err)
		}
		if id <= 0 {
			return worker.PoisonError(jsonError("delivery_id must be positive"))
		}
		return webhookrelay.Deliver(ctx, webhookrelay.DeliverDeps{
			Pool:       deps.Pool,
			Logger:     deps.Logger,
			SecretBox:  deps.SecretBox,
			SSRF:       deps.SSRF,
			HTTPClient: deps.HTTPClient,
		}, id)
	}
}
