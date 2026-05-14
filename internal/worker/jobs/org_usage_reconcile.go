// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type OrgUsageReconcileDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
}

type OrgUsageReconcilePayload struct {
	Limit        int32  `json:"limit,omitempty"`
	Source       string `json:"source,omitempty"`
	SkipSnapshot bool   `json:"skip_snapshot,omitempty"`
}

const (
	orgUsageReconcileDefaultBatch int32 = 100
	orgUsageReconcileMaxBatch     int32 = 1000
)

func OrgUsageReconcile(deps OrgUsageReconcileDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p OrgUsageReconcilePayload
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
			}
		}
		if p.Limit < 0 {
			return worker.PoisonError(errors.New("negative limit"))
		}
		limit := p.Limit
		if limit == 0 {
			limit = orgUsageReconcileDefaultBatch
		}
		if limit > orgUsageReconcileMaxBatch {
			limit = orgUsageReconcileMaxBatch
		}
		source := strings.TrimSpace(p.Source)
		if source == "" {
			source = "scheduled"
		}

		orgIDs, err := orgbilling.ListActiveOrgIDsForUsageRecalc(ctx, orgbilling.Deps{Pool: deps.Pool}, limit)
		if err != nil {
			return fmt.Errorf("list orgs for usage recalc: %w", err)
		}

		enqueued := 0
		failed := 0
		for _, orgID := range orgIDs {
			if _, err := worker.Enqueue(ctx, deps.Pool, worker.KindOrgUsageRecalc, OrgUsageRecalcPayload{
				OrgID:        orgID,
				Source:       source,
				SkipSnapshot: p.SkipSnapshot,
			}, worker.EnqueueOptions{}); err != nil {
				failed++
				if deps.Logger != nil {
					deps.Logger.WarnContext(ctx, "org usage reconcile: enqueue failed",
						"org_id", orgID, "error", err)
				}
				continue
			}
			enqueued++
		}
		if deps.Logger != nil && (enqueued > 0 || failed > 0) {
			deps.Logger.InfoContext(ctx, "org usage reconcile completed",
				"count", len(orgIDs),
				"enqueued", enqueued,
				"failed", failed,
				"limit", limit,
				"source", source,
				"skip_snapshot", p.SkipSnapshot)
		}
		return nil
	}
}
