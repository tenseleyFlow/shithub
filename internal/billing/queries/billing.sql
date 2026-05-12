-- SPDX-License-Identifier: AGPL-3.0-or-later

-- ─── org_billing_states ────────────────────────────────────────────

-- name: GetOrgBillingState :one
SELECT * FROM org_billing_states WHERE org_id = $1;

-- name: GetOrgBillingStateByStripeCustomer :one
SELECT * FROM org_billing_states
WHERE provider = 'stripe'
  AND stripe_customer_id = $1;

-- name: GetOrgBillingStateByStripeSubscription :one
SELECT * FROM org_billing_states
WHERE provider = 'stripe'
  AND stripe_subscription_id = $1;

-- name: SetStripeCustomer :one
INSERT INTO org_billing_states (org_id, provider, stripe_customer_id)
VALUES ($1, 'stripe', $2)
ON CONFLICT (org_id) DO UPDATE
   SET stripe_customer_id = EXCLUDED.stripe_customer_id,
       provider = 'stripe',
       updated_at = now()
RETURNING *;

-- name: ApplySubscriptionSnapshot :one
WITH state AS (
    INSERT INTO org_billing_states (
        org_id,
        provider,
        plan,
        subscription_status,
        stripe_subscription_id,
        stripe_subscription_item_id,
        current_period_start,
        current_period_end,
        cancel_at_period_end,
        trial_end,
        canceled_at,
        last_webhook_event_id,
        past_due_at,
        locked_at,
        lock_reason,
        grace_until
    )
    VALUES (
        sqlc.arg(org_id)::bigint,
        'stripe',
        sqlc.arg(plan)::org_plan,
        sqlc.arg(subscription_status)::billing_subscription_status,
        sqlc.narg(stripe_subscription_id)::text,
        sqlc.narg(stripe_subscription_item_id)::text,
        sqlc.narg(current_period_start)::timestamptz,
        sqlc.narg(current_period_end)::timestamptz,
        sqlc.arg(cancel_at_period_end)::boolean,
        sqlc.narg(trial_end)::timestamptz,
        sqlc.narg(canceled_at)::timestamptz,
        sqlc.arg(last_webhook_event_id)::text,
        CASE
            WHEN sqlc.arg(subscription_status)::billing_subscription_status = 'past_due' THEN now()
            ELSE NULL
        END,
        NULL,
        NULL,
        NULL
    )
    ON CONFLICT (org_id) DO UPDATE
       SET plan = EXCLUDED.plan,
           subscription_status = EXCLUDED.subscription_status,
           stripe_subscription_id = EXCLUDED.stripe_subscription_id,
           stripe_subscription_item_id = EXCLUDED.stripe_subscription_item_id,
           current_period_start = EXCLUDED.current_period_start,
           current_period_end = EXCLUDED.current_period_end,
           cancel_at_period_end = EXCLUDED.cancel_at_period_end,
           trial_end = EXCLUDED.trial_end,
           canceled_at = EXCLUDED.canceled_at,
           last_webhook_event_id = EXCLUDED.last_webhook_event_id,
           past_due_at = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN COALESCE(org_billing_states.past_due_at, now())
               ELSE NULL
           END,
           locked_at = NULL,
           lock_reason = NULL,
           grace_until = NULL,
           updated_at = now()
    RETURNING *
), org_update AS (
    UPDATE orgs
       SET plan = sqlc.arg(plan)::org_plan,
           updated_at = now()
     WHERE id = sqlc.arg(org_id)::bigint
    RETURNING id
)
SELECT * FROM state;

-- name: MarkPastDue :one
UPDATE org_billing_states
   SET subscription_status = 'past_due',
       past_due_at = COALESCE(past_due_at, now()),
       locked_at = now(),
       lock_reason = 'past_due',
       grace_until = sqlc.narg(grace_until)::timestamptz,
       last_webhook_event_id = sqlc.arg(last_webhook_event_id)::text,
       updated_at = now()
 WHERE org_id = sqlc.arg(org_id)::bigint
RETURNING *;

-- name: MarkCanceled :one
WITH state AS (
    UPDATE org_billing_states
       SET plan = 'free',
           subscription_status = 'canceled',
           canceled_at = COALESCE(canceled_at, now()),
           locked_at = now(),
           lock_reason = 'canceled',
           grace_until = NULL,
           cancel_at_period_end = false,
           last_webhook_event_id = sqlc.arg(last_webhook_event_id)::text,
           updated_at = now()
     WHERE org_id = sqlc.arg(org_id)::bigint
    RETURNING *
), org_update AS (
    UPDATE orgs
       SET plan = 'free',
           updated_at = now()
     WHERE id = sqlc.arg(org_id)::bigint
    RETURNING id
)
SELECT * FROM state;

-- name: ClearBillingLock :one
WITH state AS (
    UPDATE org_billing_states
       SET plan = CASE
               WHEN subscription_status = 'canceled' THEN 'free'
               ELSE plan
           END,
           subscription_status = CASE
               WHEN subscription_status = 'canceled' THEN 'none'
               ELSE subscription_status
           END,
           locked_at = NULL,
           lock_reason = NULL,
           grace_until = NULL,
           updated_at = now()
     WHERE org_id = $1
    RETURNING *
), org_update AS (
    UPDATE orgs
       SET plan = state.plan,
           updated_at = now()
      FROM state
     WHERE orgs.id = state.org_id
    RETURNING orgs.id
)
SELECT * FROM state;

-- ─── billing_seat_snapshots ────────────────────────────────────────

-- name: CreateSeatSnapshot :one
WITH snapshot AS (
    INSERT INTO billing_seat_snapshots (
        org_id,
        provider,
        stripe_subscription_id,
        active_members,
        billable_seats,
        source
    )
    VALUES (
        sqlc.arg(org_id)::bigint,
        'stripe',
        sqlc.narg(stripe_subscription_id)::text,
        sqlc.arg(active_members)::integer,
        sqlc.arg(billable_seats)::integer,
        sqlc.arg(source)::text
    )
    RETURNING *
), state AS (
    INSERT INTO org_billing_states (org_id, billable_seats, seat_snapshot_at)
    SELECT org_id, billable_seats, captured_at FROM snapshot
    ON CONFLICT (org_id) DO UPDATE
       SET billable_seats = EXCLUDED.billable_seats,
           seat_snapshot_at = EXCLUDED.seat_snapshot_at,
           updated_at = now()
    RETURNING org_id
)
SELECT * FROM snapshot;

-- name: ListSeatSnapshotsForOrg :many
SELECT * FROM billing_seat_snapshots
WHERE org_id = $1
ORDER BY captured_at DESC, id DESC
LIMIT $2;

-- name: CountBillableOrgMembers :one
SELECT count(*)::integer
FROM org_members
WHERE org_id = $1;

-- ─── billing_invoices ──────────────────────────────────────────────

-- name: UpsertInvoice :one
INSERT INTO billing_invoices (
    org_id,
    provider,
    stripe_invoice_id,
    stripe_customer_id,
    stripe_subscription_id,
    status,
    number,
    currency,
    amount_due_cents,
    amount_paid_cents,
    amount_remaining_cents,
    hosted_invoice_url,
    invoice_pdf_url,
    period_start,
    period_end,
    due_at,
    paid_at,
    voided_at
)
VALUES (
    sqlc.arg(org_id)::bigint,
    'stripe',
    sqlc.arg(stripe_invoice_id)::text,
    sqlc.arg(stripe_customer_id)::text,
    sqlc.narg(stripe_subscription_id)::text,
    sqlc.arg(status)::billing_invoice_status,
    sqlc.arg(number)::text,
    sqlc.arg(currency)::text,
    sqlc.arg(amount_due_cents)::bigint,
    sqlc.arg(amount_paid_cents)::bigint,
    sqlc.arg(amount_remaining_cents)::bigint,
    sqlc.arg(hosted_invoice_url)::text,
    sqlc.arg(invoice_pdf_url)::text,
    sqlc.narg(period_start)::timestamptz,
    sqlc.narg(period_end)::timestamptz,
    sqlc.narg(due_at)::timestamptz,
    sqlc.narg(paid_at)::timestamptz,
    sqlc.narg(voided_at)::timestamptz
)
ON CONFLICT (provider, stripe_invoice_id) DO UPDATE
   SET org_id = EXCLUDED.org_id,
       stripe_customer_id = EXCLUDED.stripe_customer_id,
       stripe_subscription_id = EXCLUDED.stripe_subscription_id,
       status = EXCLUDED.status,
       number = EXCLUDED.number,
       currency = EXCLUDED.currency,
       amount_due_cents = EXCLUDED.amount_due_cents,
       amount_paid_cents = EXCLUDED.amount_paid_cents,
       amount_remaining_cents = EXCLUDED.amount_remaining_cents,
       hosted_invoice_url = EXCLUDED.hosted_invoice_url,
       invoice_pdf_url = EXCLUDED.invoice_pdf_url,
       period_start = EXCLUDED.period_start,
       period_end = EXCLUDED.period_end,
       due_at = EXCLUDED.due_at,
       paid_at = EXCLUDED.paid_at,
       voided_at = EXCLUDED.voided_at,
       updated_at = now()
RETURNING *;

-- name: ListInvoicesForOrg :many
SELECT * FROM billing_invoices
WHERE org_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- ─── billing_webhook_events ────────────────────────────────────────

-- name: CreateWebhookEventReceipt :one
INSERT INTO billing_webhook_events (
    provider,
    provider_event_id,
    event_type,
    api_version,
    payload
)
VALUES (
    'stripe',
    sqlc.arg(provider_event_id)::text,
    sqlc.arg(event_type)::text,
    sqlc.arg(api_version)::text,
    sqlc.arg(payload)::jsonb
)
ON CONFLICT (provider, provider_event_id) DO NOTHING
RETURNING *;

-- name: GetWebhookEventReceipt :one
SELECT * FROM billing_webhook_events
WHERE provider = 'stripe'
  AND provider_event_id = $1;

-- name: MarkWebhookEventProcessed :one
UPDATE billing_webhook_events
   SET processed_at = now(),
       process_error = '',
       processing_attempts = processing_attempts + 1
 WHERE provider = 'stripe'
   AND provider_event_id = $1
RETURNING *;

-- name: MarkWebhookEventFailed :one
UPDATE billing_webhook_events
   SET process_error = $2,
       processing_attempts = processing_attempts + 1
 WHERE provider = 'stripe'
   AND provider_event_id = $1
RETURNING *;
