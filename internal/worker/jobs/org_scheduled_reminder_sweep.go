// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"

	"github.com/tenseleyFlow/shithub/internal/orgs"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type OrgScheduledReminderSweepDeps = orgs.ScheduledReminderSweepDeps

func OrgScheduledReminderSweep(deps OrgScheduledReminderSweepDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		processed, err := orgs.SweepScheduledReminders(ctx, deps)
		if err != nil {
			return err
		}
		if deps.Logger != nil && processed > 0 {
			deps.Logger.InfoContext(ctx, "org:scheduled_reminder_sweep drained rows",
				"count", processed)
		}
		if processed >= orgs.ScheduledReminderSweepBatch {
			if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindOrgScheduledReminderSweep,
				map[string]any{}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "org:scheduled_reminder_sweep self-enqueue failed",
					"error", err)
			}
		}
		return nil
	}
}
