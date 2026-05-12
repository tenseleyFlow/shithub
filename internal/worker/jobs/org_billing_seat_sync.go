// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	orgbilling "github.com/tenseleyFlow/shithub/internal/billing"
	"github.com/tenseleyFlow/shithub/internal/billing/stripebilling"
	"github.com/tenseleyFlow/shithub/internal/worker"
)

type OrgBillingSeatSyncDeps struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	Stripe stripebilling.Remote
}

type OrgBillingSeatSyncPayload struct {
	OrgID int64 `json:"org_id"`
}

func OrgBillingSeatSync(deps OrgBillingSeatSyncDeps) worker.Handler {
	return func(ctx context.Context, raw json.RawMessage) error {
		var p OrgBillingSeatSyncPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			return worker.PoisonError(fmt.Errorf("bad payload: %w", err))
		}
		if p.OrgID == 0 {
			return worker.PoisonError(errors.New("missing org_id"))
		}

		bdeps := orgbilling.Deps{Pool: deps.Pool}
		state, err := orgbilling.GetOrgBillingState(ctx, bdeps, p.OrgID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				if deps.Logger != nil {
					deps.Logger.InfoContext(ctx, "org billing seat sync skipped; billing state missing",
						"org_id", p.OrgID)
				}
				return nil
			}
			return fmt.Errorf("load billing state: %w", err)
		}

		members, err := orgbilling.CountBillableOrgMembers(ctx, bdeps, p.OrgID)
		if err != nil {
			return fmt.Errorf("count billable members: %w", err)
		}
		if _, err := orgbilling.SyncSeatSnapshot(ctx, bdeps, orgbilling.SeatSnapshot{
			OrgID:                p.OrgID,
			StripeSubscriptionID: state.StripeSubscriptionID.String,
			ActiveMembers:        members,
			BillableSeats:        members,
			Source:               "worker",
		}); err != nil {
			return fmt.Errorf("sync seat snapshot: %w", err)
		}

		if deps.Stripe == nil || !shouldSyncStripeSeatQuantity(state) {
			return nil
		}
		if err := deps.Stripe.UpdateSubscriptionItemQuantity(ctx, stripebilling.SeatQuantityInput{
			OrgID:              p.OrgID,
			SubscriptionItemID: state.StripeSubscriptionItemID.String,
			Quantity:           int64(members),
		}); err != nil {
			return fmt.Errorf("update stripe subscription item quantity: %w", err)
		}
		if deps.Logger != nil {
			deps.Logger.InfoContext(ctx, "org billing seat sync updated subscription quantity",
				"org_id", p.OrgID,
				"seats", members,
				"subscription_item_id", state.StripeSubscriptionItemID.String)
		}
		return nil
	}
}

func shouldSyncStripeSeatQuantity(state orgbilling.State) bool {
	if !state.StripeSubscriptionItemID.Valid {
		return false
	}
	switch state.SubscriptionStatus {
	case orgbilling.SubscriptionStatusActive,
		orgbilling.SubscriptionStatusTrialing,
		orgbilling.SubscriptionStatusIncomplete,
		orgbilling.SubscriptionStatusPastDue,
		orgbilling.SubscriptionStatusUnpaid,
		orgbilling.SubscriptionStatusPaused:
		return true
	default:
		return false
	}
}
