// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/secretbox"
	"github.com/tenseleyFlow/shithub/internal/webhook"
	webhookdb "github.com/tenseleyFlow/shithub/internal/webhook/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// WebhookFanoutDeps wires the fan-out handler.
type WebhookFanoutDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// WebhookFanout drains domain_events past the webhook cursor and
// creates per-subscriber delivery rows + deliver jobs. Self-throttles:
// when a tick drains a full batch, the handler re-enqueues itself so
// the next tick keeps draining without waiting for the cron beat.
func WebhookFanout(deps WebhookFanoutDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		processed, err := webhook.FanoutOnce(ctx, webhook.FanoutDeps{
			Pool: deps.Pool, Logger: deps.Logger,
		})
		if err != nil {
			return err
		}
		if deps.Logger != nil && processed > 0 {
			deps.Logger.InfoContext(ctx, "webhook:fanout drained events",
				"count", processed)
		}
		if processed >= webhook.FanoutBatch {
			if _, err := worker.Enqueue(ctx, deps.Pool, webhook.KindWebhookFanout,
				map[string]any{}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "webhook:fanout self-enqueue failed",
					"error", err)
			}
		}
		return nil
	}
}

// WebhookDeliverDeps wires the deliverer. SecretBox is the AEAD box
// from cfg.Webhook.AEADKey; the pre-separation auth.totp_key_b64
// fallback was retired in F-shakedown after prod confirmed zero
// rows remained on the legacy key.
type WebhookDeliverDeps struct {
	Pool      *pgxpool.Pool
	Logger    *slog.Logger
	SecretBox *secretbox.Box
	SSRF      webhook.SSRFConfig
}

// WebhookDeliver dispatches one delivery row. The job kind is fired by
// fanout (1 row → 1 job); a row for which the deliverer schedules a
// retry will get re-enqueued through next-retry-at scheduling — see
// the cron-driven webhook:retry sweep documented in webhook docs.
func WebhookDeliver(deps WebhookDeliverDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		id, err := webhook.UnmarshalDeliverPayload(raw)
		if err != nil {
			return worker.PoisonError(err)
		}
		if id <= 0 {
			return worker.PoisonError(jsonError("delivery_id must be positive"))
		}
		return webhook.Deliver(ctx, webhook.DeliverDeps{
			Pool:      deps.Pool,
			Logger:    deps.Logger,
			SecretBox: deps.SecretBox,
			SSRF:      deps.SSRF,
		}, id)
	}
}

// WebhookPurgeOldDeps wires the retention sweep.
type WebhookPurgeOldDeps struct {
	Pool      *pgxpool.Pool
	Logger    *slog.Logger
	Retention time.Duration
}

// WebhookPurgeOld drops terminal delivery rows older than the
// retention window. Cron-driven; pending/retry rows are left alone.
func WebhookPurgeOld(deps WebhookPurgeOldDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		retention := deps.Retention
		if retention <= 0 {
			retention = 30 * 24 * time.Hour
		}
		ivl := pgtype.Interval{
			Microseconds: int64(retention / time.Microsecond),
			Valid:        true,
		}
		_, err := webhookdb.New().PurgeOldDeliveries(ctx, deps.Pool, ivl)
		return err
	}
}

// jsonError is a tiny error wrapper so the poison wrap above keeps
// reading naturally. Its message is preserved in last_error.
type jsonErr string

func (e jsonErr) Error() string { return string(e) }
func jsonError(s string) error  { return jsonErr(s) }
