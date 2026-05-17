// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"

	"github.com/tenseleyFlow/shithub/internal/notifications"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// NotifyDigestSweep returns the worker handler for the digest sweep
// (PRO-EXT01-16b). Mirrors the cron-workflow sweep shape: claim a
// bounded batch of due rows, send each digest email, advance
// next_send_at. On a full batch, re-enqueue ourselves so the next
// tick processes the remainder without waiting for the systemd
// timer's next beat.
//
// The handler is a thin shim — all of the cadence + email logic
// lives in internal/notifications/digest.go where it's unit-testable
// against synthetic times.
func NotifyDigestSweep(deps notifications.DigestSweepDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		processed, err := notifications.SweepOnce(ctx, deps)
		if err != nil {
			return err
		}
		if deps.Logger != nil && processed > 0 {
			deps.Logger.InfoContext(ctx, "notify:digest_sweep drained rows",
				"count", processed)
		}
		if processed >= notifications.DigestSweepBatch {
			if _, err := worker.Enqueue(ctx, deps.Pool, notifications.KindNotifyDigestSweep,
				map[string]any{}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "notify:digest_sweep self-enqueue failed",
					"error", err)
			}
		}
		return nil
	}
}
