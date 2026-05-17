// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

// PRO-EXT01-10d: thin worker.Handler wrapper around
// secretscan.DispatchAlert. The real dispatch lives in the secretscan
// package so the alert flow is testable in-process without spinning up
// the worker pool — this file just bridges the two type shapes.

import (
	"context"
	"encoding/json"

	"github.com/tenseleyFlow/shithub/internal/secretscan"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

func SecretScanAlertDispatch(deps secretscan.AlertDeps) worker.Handler {
	disp := secretscan.DispatchAlert(deps)
	return func(ctx context.Context, raw json.RawMessage) error {
		return disp(ctx, raw)
	}
}
