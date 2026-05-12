// SPDX-License-Identifier: AGPL-3.0-or-later

package orgs

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/tenseleyFlow/shithub/internal/worker"
)

func enqueueBillingSeatSync(ctx context.Context, tx pgx.Tx, deps Deps, orgID int64) error {
	if _, err := worker.Enqueue(ctx, tx, worker.KindOrgBillingSeatSync, map[string]any{
		"org_id": orgID,
	}, worker.EnqueueOptions{}); err != nil {
		return err
	}
	if err := worker.Notify(ctx, tx); err != nil && deps.Logger != nil {
		deps.Logger.WarnContext(ctx, "org billing: notify seat sync", "error", err, "org_id", orgID)
	}
	return nil
}
