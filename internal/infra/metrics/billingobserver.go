// SPDX-License-Identifier: AGPL-3.0-or-later

package metrics

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tenseleyFlow/shithub/internal/entitlements"
)

// ObserveBilling starts a goroutine that periodically refreshes DB-backed
// billing gauges. The goroutine exits when ctx is canceled.
func ObserveBilling(ctx context.Context, pool *pgxpool.Pool, interval time.Duration) {
	if pool == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	go func() {
		refreshBilling(ctx, pool)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refreshBilling(ctx, pool)
			}
		}
	}()
}

func refreshBilling(ctx context.Context, pool *pgxpool.Pool) {
	if pool == nil {
		return
	}
	refreshBillingWebhookBacklog(ctx, pool)
	refreshBillingPastDuePrincipals(ctx, pool)
	refreshBillingOrgSeatDrift(ctx, pool)
	refreshBillingQuotaOverages(ctx, pool)
}

func refreshBillingWebhookBacklog(ctx context.Context, pool *pgxpool.Pool) {
	BillingWebhookBacklog.WithLabelValues("pending").Set(0)
	BillingWebhookBacklog.WithLabelValues("failed").Set(0)
	var pending, failed float64
	if err := pool.QueryRow(ctx, `
SELECT
  count(*) FILTER (WHERE processed_at IS NULL)::double precision AS pending,
  count(*) FILTER (WHERE processed_at IS NULL AND process_error IS NOT NULL)::double precision AS failed
FROM billing_webhook_events`).Scan(&pending, &failed); err != nil {
		return
	}
	BillingWebhookBacklog.WithLabelValues("pending").Set(pending)
	BillingWebhookBacklog.WithLabelValues("failed").Set(failed)
}

func refreshBillingPastDuePrincipals(ctx context.Context, pool *pgxpool.Pool) {
	BillingPastDuePrincipals.WithLabelValues("org").Set(0)
	BillingPastDuePrincipals.WithLabelValues("user").Set(0)
	rows, err := pool.Query(ctx, `
SELECT 'org'::text AS subject_kind, count(*)::double precision
FROM org_billing_states
WHERE subscription_status IN ('past_due', 'unpaid', 'incomplete')
UNION ALL
SELECT 'user'::text AS subject_kind, count(*)::double precision
FROM user_billing_states
WHERE subscription_status IN ('past_due', 'unpaid', 'incomplete')`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var count float64
		if err := rows.Scan(&kind, &count); err != nil {
			return
		}
		BillingPastDuePrincipals.WithLabelValues(kind).Set(count)
	}
}

func refreshBillingOrgSeatDrift(ctx context.Context, pool *pgxpool.Pool) {
	BillingOrgSeatDrift.Set(0)
	var drift float64
	if err := pool.QueryRow(ctx, `
WITH seat_counts AS (
  SELECT org_id, count(*)::bigint AS seats
  FROM org_members
  GROUP BY org_id
)
SELECT count(*)::double precision
FROM org_billing_states s
LEFT JOIN seat_counts c ON c.org_id = s.org_id
WHERE s.plan = 'team'
  AND s.subscription_status IN ('active', 'trialing', 'past_due', 'unpaid', 'incomplete', 'paused')
  AND s.stripe_subscription_item_id IS NOT NULL
  AND s.billable_seats <> COALESCE(c.seats, 0)`).Scan(&drift); err != nil {
		return
	}
	BillingOrgSeatDrift.Set(drift)
}

func refreshBillingQuotaOverages(ctx context.Context, pool *pgxpool.Pool) {
	BillingQuotaOverageOrgs.WithLabelValues("storage_bytes").Set(0)
	BillingQuotaOverageOrgs.WithLabelValues("actions_minutes").Set(0)
	var storage, actions float64
	if err := pool.QueryRow(ctx, `
WITH usage_limits AS (
  SELECT
    c.org_id,
    c.repo_storage_bytes + c.object_storage_bytes AS storage_bytes,
    c.actions_minutes_used,
    CASE
      WHEN storage_override.unlimited THEN NULL
      ELSE COALESCE(
        storage_override.limit_value,
        CASE
          WHEN s.plan = 'team'
           AND (
             s.subscription_status IN ('active', 'trialing')
             OR (s.subscription_status = 'past_due' AND s.grace_until IS NOT NULL AND s.grace_until >= now())
           )
          THEN $2::bigint
          ELSE $1::bigint
        END
      )
    END AS storage_limit,
    CASE
      WHEN actions_override.unlimited THEN NULL
      ELSE COALESCE(
        actions_override.limit_value,
        CASE
          WHEN s.plan = 'team'
           AND (
             s.subscription_status IN ('active', 'trialing')
             OR (s.subscription_status = 'past_due' AND s.grace_until IS NOT NULL AND s.grace_until >= now())
           )
          THEN $4::bigint
          ELSE $3::bigint
        END
      )
    END AS actions_limit
  FROM org_usage_counters c
  LEFT JOIN org_billing_states s ON s.org_id = c.org_id
  LEFT JOIN org_quota_overrides storage_override
    ON storage_override.org_id = c.org_id AND storage_override.kind = 'storage_bytes'
  LEFT JOIN org_quota_overrides actions_override
    ON actions_override.org_id = c.org_id AND actions_override.kind = 'actions_minutes'
)
SELECT
  count(*) FILTER (WHERE storage_limit IS NOT NULL AND storage_bytes > storage_limit)::double precision AS storage_over,
  count(*) FILTER (WHERE actions_limit IS NOT NULL AND actions_minutes_used > actions_limit)::double precision AS actions_over
FROM usage_limits`,
		entitlements.FreeOrgStorageQuotaBytes,
		entitlements.TeamOrgStorageQuotaBytes,
		entitlements.FreeOrgActionsMinutesQuota,
		entitlements.TeamOrgActionsMinutesQuota,
	).Scan(&storage, &actions); err != nil {
		return
	}
	BillingQuotaOverageOrgs.WithLabelValues("storage_bytes").Set(storage)
	BillingQuotaOverageOrgs.WithLabelValues("actions_minutes").Set(actions)
}
