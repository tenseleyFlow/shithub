// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/auth/email"
	"github.com/tenseleyFlow/shithub/internal/notif"
	"github.com/tenseleyFlow/shithub/internal/notifications"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// NotifyFanoutDeps wires the fan-out handler against the runtime.
// EmailSender is optional — when nil, inbox rows still get written
// but no email goes out (matches the dev-mode "look at the inbox in
// the web UI" loop).
type NotifyFanoutDeps struct {
	Pool           *pgxpool.Pool
	Logger         *slog.Logger
	EmailSender    email.Sender
	EmailFrom      string
	SiteName       string
	BaseURL        string
	UnsubscribeKey []byte
	// EnforceInboxRules promotes the PRO-EXT01-16a rule engine from
	// report-only to enforce: Free users' rules stop firing entirely
	// (instead of firing-with-deny-log). Operator-controlled via
	// EnforceConfig.UserInboxRules.
	EnforceInboxRules bool
}

// NotifyFanout returns the worker handler for `notify:fanout`. The
// handler ignores the payload (cursor lives in
// domain_events_processed) and drains up to FanoutBatch events per
// invocation. The pool re-runs the job through normal retry cadence
// when a drain partially fails, and the cron-driven scheduler re-
// enqueues on a tick.
func NotifyFanout(deps NotifyFanoutDeps) worker.Handler {
	engine := &notifications.Evaluator{
		Pool:        deps.Pool,
		Logger:      deps.Logger,
		EnforceFree: deps.EnforceInboxRules,
	}
	return func(ctx context.Context, _ json.RawMessage) error {
		processed, err := notif.FanoutOnce(ctx, notif.Deps{
			Pool:           deps.Pool,
			Logger:         deps.Logger,
			EmailSender:    deps.EmailSender,
			EmailFrom:      deps.EmailFrom,
			SiteName:       deps.SiteName,
			BaseURL:        deps.BaseURL,
			UnsubscribeKey: deps.UnsubscribeKey,
			RuleEngine:     engine,
		})
		if err != nil {
			return err
		}
		if deps.Logger != nil && processed > 0 {
			deps.Logger.InfoContext(ctx, "notify:fanout drained events",
				"count", processed)
		}
		// If we drained a full batch, more events probably exist.
		// Re-enqueue ourselves so we keep going on the next tick
		// without waiting for the next cron beat. Best-effort: an
		// enqueue failure is logged but not surfaced as a job error
		// (the next cron tick will pick up).
		if processed >= notif.FanoutBatch {
			if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindNotifyFanout,
				map[string]any{}, worker.EnqueueOptions{}); err != nil && deps.Logger != nil {
				deps.Logger.WarnContext(ctx, "notify:fanout self-enqueue failed",
					"error", err)
			}
		}
		return nil
	}
}
