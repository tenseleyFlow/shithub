// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	usersdb "github.com/tenseleyFlow/shithub/internal/users/sqlc"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

// DeviceAuthorizationSweepDeps wires the daily devicecode retention job.
type DeviceAuthorizationSweepDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

// DeviceAuthorizationSweep deletes terminal/expired
// device_authorizations rows older than the 24h forensics window. The
// SQL lives in DeleteExpiredDeviceAuthorizations
// (internal/users/queries/device_authorizations.sql); we hand the
// scheduling decision to the operator's external timer rather than
// building cron into the worker, matching the WebhookPurgeOld pattern
// in webhook.go and the CronWorkflowSweep pattern in cron_workflow.go.
func DeviceAuthorizationSweep(deps DeviceAuthorizationSweepDeps) worker.Handler {
	return func(ctx context.Context, _ json.RawMessage) error {
		if err := usersdb.New().DeleteExpiredDeviceAuthorizations(ctx, deps.Pool); err != nil {
			return err
		}
		if deps.Logger != nil {
			deps.Logger.InfoContext(ctx, "devicecode.sweep_complete")
		}
		return nil
	}
}
