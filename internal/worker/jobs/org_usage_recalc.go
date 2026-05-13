// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type OrgUsageRecalcDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Now    func() time.Time
}

type OrgUsageRecalcPayload struct {
	OrgID        int64  `json:"org_id"`
	Source       string `json:"source,omitempty"`
	SkipSnapshot bool   `json:"skip_snapshot,omitempty"`
}

func OrgUsageRecalc(deps OrgUsageRecalcDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p OrgUsageRecalcPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.OrgID == 0 {
			return worker.PoisonError(errors.New("missing org_id"))
		}

		bdeps := orgbilling.Deps{Pool: deps.Pool}
		if _, err := orgbilling.GetOrgBillingState(ctx, bdeps, p.OrgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if deps.Logger != nil {
					deps.Logger.InfoContext(ctx, "org usage recalc skipped; billing state missing",
						"org_id", p.OrgID)
				}
				return nil
			}
			return fmt.Errorf("load billing state: %w", err)
		}

		now := time.Now().UTC()
		if deps.Now != nil {
			now = deps.Now().UTC()
		}
		periodStart, periodEnd := orgbilling.MonthlyUsagePeriod(now)
		counters, err := orgbilling.RecalculateOrgUsageCounters(ctx, bdeps, p.OrgID, periodStart, periodEnd)
		if err != nil {
			return fmt.Errorf("recalculate usage counters: %w", err)
		}
		source := strings.TrimSpace(p.Source)
		if source == "" {
			source = "worker"
		}
		if !p.SkipSnapshot {
			if _, err := orgbilling.CreateOrgUsageSnapshot(ctx, bdeps, p.OrgID, source); err != nil {
				return fmt.Errorf("create usage snapshot: %w", err)
			}
		}
		if deps.Logger != nil {
			deps.Logger.InfoContext(ctx, "org usage recalc completed",
				"org_id", p.OrgID,
				"repo_storage_bytes", counters.RepoStorageBytes,
				"object_storage_bytes", counters.ObjectStorageBytes,
				"actions_minutes_used", counters.ActionsMinutesUsed,
				"period_start", periodStart,
				"period_end", periodEnd)
		}
		return nil
	}
}
