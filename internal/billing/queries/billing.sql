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
WITH seat_counts AS (
    SELECT count(*)::integer AS used_seats
    FROM org_members
    WHERE org_id = sqlc.arg(org_id)::bigint
), state AS (
    INSERT INTO org_billing_states (
        org_id,
        provider,
        plan,
        subscription_status,
        billable_seats,
        licensed_seats,
        used_seats,
        seat_snapshot_at,
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
        CASE
            WHEN sqlc.arg(plan)::org_plan = 'team' THEN GREATEST((SELECT used_seats FROM seat_counts), 1)
            ELSE 0
        END,
        CASE
            WHEN sqlc.arg(plan)::org_plan = 'team' THEN GREATEST((SELECT used_seats FROM seat_counts), 1)
            ELSE 0
        END,
        (SELECT used_seats FROM seat_counts),
        now(),
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
           billable_seats = CASE
               WHEN EXCLUDED.plan = 'team' THEN GREATEST(
                   org_billing_states.billable_seats,
                   org_billing_states.licensed_seats,
                   EXCLUDED.used_seats,
                   1
               )
               ELSE 0
           END,
           licensed_seats = CASE
               WHEN EXCLUDED.plan = 'team' THEN GREATEST(
                   org_billing_states.licensed_seats,
                   org_billing_states.billable_seats,
                   EXCLUDED.used_seats,
                   1
               )
               ELSE 0
           END,
           used_seats = EXCLUDED.used_seats,
           seat_snapshot_at = EXCLUDED.seat_snapshot_at,
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
           -- PRO08 D1: never unconditionally NULL the lock columns.
           --   past_due -> preserve any existing lock (MarkPastDue
           --     sets fresh grace_until on the invoice.payment_failed
           --     path; if that hasn't arrived yet, leave NULL).
           --   active / trialing recovering from past_due/unpaid -> clear.
           --   any other transition -> preserve existing values.
           locked_at = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN COALESCE(org_billing_states.locked_at, now())
               WHEN EXCLUDED.subscription_status IN ('active', 'trialing')
                    AND org_billing_states.subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE org_billing_states.locked_at
           END,
           lock_reason = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN COALESCE(org_billing_states.lock_reason, 'past_due'::billing_lock_reason)
               WHEN EXCLUDED.subscription_status IN ('active', 'trialing')
                    AND org_billing_states.subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE org_billing_states.lock_reason
           END,
           grace_until = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN org_billing_states.grace_until
               WHEN EXCLUDED.subscription_status IN ('active', 'trialing')
                    AND org_billing_states.subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE org_billing_states.grace_until
           END,
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

-- name: MarkPaymentSucceeded :one
WITH state AS (
    UPDATE org_billing_states
       SET plan = CASE
               WHEN subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN 'team'
               ELSE plan
           END,
           subscription_status = CASE
               WHEN subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN 'active'
               ELSE subscription_status
           END,
           past_due_at = CASE
               WHEN subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE past_due_at
           END,
           locked_at = NULL,
           lock_reason = NULL,
           grace_until = NULL,
           last_webhook_event_id = sqlc.arg(last_webhook_event_id)::text,
           updated_at = now()
     WHERE org_id = sqlc.arg(org_id)::bigint
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
        licensed_seats,
        used_seats,
        available_seats,
        source
    )
    VALUES (
        sqlc.arg(org_id)::bigint,
        'stripe',
        sqlc.narg(stripe_subscription_id)::text,
        sqlc.arg(used_seats)::integer,
        sqlc.arg(licensed_seats)::integer,
        sqlc.arg(licensed_seats)::integer,
        sqlc.arg(used_seats)::integer,
        GREATEST(sqlc.arg(licensed_seats)::integer - sqlc.arg(used_seats)::integer, 0),
        sqlc.arg(source)::text
    )
    RETURNING *
), state AS (
    INSERT INTO org_billing_states (org_id, billable_seats, licensed_seats, used_seats, seat_snapshot_at)
    SELECT org_id, licensed_seats, licensed_seats, used_seats, captured_at FROM snapshot
    ON CONFLICT (org_id) DO UPDATE
       SET billable_seats = EXCLUDED.billable_seats,
           licensed_seats = EXCLUDED.licensed_seats,
           used_seats = EXCLUDED.used_seats,
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

-- name: CountPendingOrgInvitations :one
SELECT count(*)::integer
FROM org_invitations
WHERE org_id = $1
  AND accepted_at IS NULL
  AND declined_at IS NULL
  AND canceled_at IS NULL
  AND expires_at > now();

-- ─── billing_invoices ──────────────────────────────────────────────

-- name: UpsertInvoice :one
-- PRO03: writes both legacy `org_id` and polymorphic
-- `(subject_kind, subject_id)`. Callers continue to bind org_id only;
-- the subject columns are derived. After PRO04 migrates all callers
-- to the polymorphic shape, a follow-up migration drops `org_id` and
-- this query loses the legacy column from its INSERT list.
INSERT INTO billing_invoices (
    org_id,
    subject_kind,
    subject_id,
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
    'org'::billing_subject_kind,
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
-- PRO03: filters on the polymorphic subject columns so the index
-- billing_invoices_subject_created_idx services this query. The
-- legacy `org_id` column is kept populated by UpsertInvoice for the
-- transitional window; this query no longer reads it.
SELECT * FROM billing_invoices
WHERE subject_kind = 'org' AND subject_id = $1
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListInvoicesForSubject :many
-- Polymorphic invoice listing for PRO04+ callers. The org-flavored
-- ListInvoicesForOrg above is the same query with subject_kind
-- hard-coded; this surface lets a user-side caller pass kind='user'
-- without forking the helper.
SELECT * FROM billing_invoices
WHERE subject_kind = sqlc.arg(subject_kind)::billing_subject_kind
  AND subject_id = sqlc.arg(subject_id)::bigint
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(lim)::integer;

-- name: UpsertInvoiceForSubject :one
-- PRO04 polymorphic invoice upsert. Writes (subject_kind,
-- subject_id) directly; org_id stays NULL for user-kind rows (per
-- the 0074 migration's nullable change). The existing
-- UpsertInvoice query stays as the org-kind path during the
-- transitional deploy — both can coexist because the UNIQUE
-- (provider, stripe_invoice_id) prevents duplicate rows.
INSERT INTO billing_invoices (
    subject_kind,
    subject_id,
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
    sqlc.arg(subject_kind)::billing_subject_kind,
    sqlc.arg(subject_id)::bigint,
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
   SET subject_kind = EXCLUDED.subject_kind,
       subject_id = EXCLUDED.subject_id,
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

-- name: SetWebhookEventSubject :exec
-- Records the resolved subject on the receipt row after a successful
-- subject-resolution step. Called from the apply path before guard +
-- state mutation so the receipt carries the audit trail even if the
-- subsequent apply fails. Migration 0075's CHECK constraint enforces
-- both-or-neither; callers must pass a non-zero subject.
UPDATE billing_webhook_events
   SET subject_kind = sqlc.arg(subject_kind)::billing_subject_kind,
       subject_id   = sqlc.arg(subject_id)::bigint
 WHERE provider = 'stripe'
   AND provider_event_id = sqlc.arg(provider_event_id)::text;

-- name: IsOrgBillingEventStale :one
-- PRO08 D4: returns true when an incoming Stripe event's timestamp
-- is older than the last event we've already applied for this org.
-- Stripe doesn't guarantee delivery order across retries; without
-- this guard a stale `subscription.updated[active]` could re-activate
-- a canceled subscription. Returns false when no prior event has
-- been recorded (last_event_at IS NULL) — the first event is never
-- stale.
SELECT COALESCE(last_event_at > sqlc.arg(event_at)::timestamptz, false)::boolean AS stale
  FROM org_billing_states
 WHERE org_id = sqlc.arg(org_id)::bigint;

-- name: IsUserBillingEventStale :one
SELECT COALESCE(last_event_at > sqlc.arg(event_at)::timestamptz, false)::boolean AS stale
  FROM user_billing_states
 WHERE user_id = sqlc.arg(user_id)::bigint;

-- name: TouchOrgBillingLastEventAt :exec
-- PRO08 D4: bump last_event_at on successful apply. Conditional so
-- a fresh apply driven by an out-of-order-but-recent retry doesn't
-- regress the timestamp (GREATEST). NULL last_event_at acquires the
-- incoming value.
UPDATE org_billing_states
   SET last_event_at = GREATEST(COALESCE(last_event_at, sqlc.arg(event_at)::timestamptz), sqlc.arg(event_at)::timestamptz)
 WHERE org_id = sqlc.arg(org_id)::bigint;

-- name: TouchUserBillingLastEventAt :exec
UPDATE user_billing_states
   SET last_event_at = GREATEST(COALESCE(last_event_at, sqlc.arg(event_at)::timestamptz), sqlc.arg(event_at)::timestamptz)
 WHERE user_id = sqlc.arg(user_id)::bigint;

-- name: MarkInvoiceRefunded :one
-- PRO08 D2: surface a Stripe-side refund in shithub. Stripe leaves
-- the invoice.status='paid' after a refund and fires a charge.refunded
-- event; this helper flips the shithub-side row to 'refunded' so the
-- billing settings UI shows the refunded state.
--
-- A NULL refunded_at means "no refund seen"; the value is set on the
-- first call and preserved on subsequent calls (refund partial → full
-- doesn't move the wall-clock timestamp).
UPDATE billing_invoices
   SET status = 'refunded',
       refunded_at = COALESCE(refunded_at, now()),
       updated_at = now()
 WHERE provider = 'stripe'
   AND stripe_invoice_id = sqlc.arg(stripe_invoice_id)::text
RETURNING *;

-- name: TryAcquireWebhookEventLock :one
-- PRO08 A3: transaction-scoped advisory lock keyed on the hash of
-- the provider_event_id. Two concurrent webhook deliveries for the
-- same event_id race past CreateWebhookEventReceipt before either has
-- marked it processed; without serialization, both proceed to apply
-- and double-mutate state. This lock makes the apply path mutually
-- exclusive per event. Returns true when acquired; false means
-- another worker holds it — caller should let Stripe retry.
--
-- pg_try_advisory_xact_lock takes a bigint; hashtext returns int4
-- which sign-extends safely. The lock auto-releases at txn end.
SELECT pg_try_advisory_xact_lock(hashtext($1)::bigint) AS acquired;

-- name: ListFailedWebhookEvents :many
-- Operator query for "events we received but failed to process."
-- A row is "failed" when it has a non-empty process_error OR when
-- it has never been processed (processed_at NULL) and has at least
-- one processing attempt. Rows that are merely new and untouched
-- (attempts=0, processed_at NULL, no error) are excluded.
SELECT id, provider, provider_event_id, event_type, api_version,
       received_at, processed_at, processing_attempts, process_error,
       subject_kind, subject_id
  FROM billing_webhook_events
 WHERE provider = 'stripe'
   AND (
        process_error <> ''
        OR (processed_at IS NULL AND processing_attempts > 0)
       )
 ORDER BY received_at DESC
 LIMIT $1;

-- ─── user_billing_states (PRO03) ──────────────────────────────────

-- name: GetUserBillingState :one
SELECT * FROM user_billing_states WHERE user_id = $1;

-- name: GetUserBillingStateByStripeCustomer :one
SELECT * FROM user_billing_states
WHERE provider = 'stripe'
  AND stripe_customer_id = $1;

-- name: GetUserBillingStateByStripeSubscription :one
SELECT * FROM user_billing_states
WHERE provider = 'stripe'
  AND stripe_subscription_id = $1;

-- name: SetUserStripeCustomer :one
INSERT INTO user_billing_states (user_id, provider, stripe_customer_id)
VALUES ($1, 'stripe', $2)
ON CONFLICT (user_id) DO UPDATE
   SET stripe_customer_id = EXCLUDED.stripe_customer_id,
       provider = 'stripe',
       updated_at = now()
RETURNING *;

-- name: ApplyUserSubscriptionSnapshot :one
-- Mirrors ApplySubscriptionSnapshot for orgs minus the seat columns
-- and with `user_plan` as the plan enum. The same CTE pattern keeps
-- users.plan and user_billing_states.plan atomic.
WITH state AS (
    INSERT INTO user_billing_states (
        user_id,
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
        sqlc.arg(user_id)::bigint,
        'stripe',
        sqlc.arg(plan)::user_plan,
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
    ON CONFLICT (user_id) DO UPDATE
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
               WHEN EXCLUDED.subscription_status = 'past_due' THEN COALESCE(user_billing_states.past_due_at, now())
               ELSE NULL
           END,
           -- PRO08 D1: never unconditionally NULL the lock columns
           -- (mirror of the org-side fix). The Mark* paths own
           -- transitions into/out of the locked state.
           locked_at = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN COALESCE(user_billing_states.locked_at, now())
               WHEN EXCLUDED.subscription_status IN ('active', 'trialing')
                    AND user_billing_states.subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE user_billing_states.locked_at
           END,
           lock_reason = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN COALESCE(user_billing_states.lock_reason, 'past_due'::billing_lock_reason)
               WHEN EXCLUDED.subscription_status IN ('active', 'trialing')
                    AND user_billing_states.subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE user_billing_states.lock_reason
           END,
           grace_until = CASE
               WHEN EXCLUDED.subscription_status = 'past_due' THEN user_billing_states.grace_until
               WHEN EXCLUDED.subscription_status IN ('active', 'trialing')
                    AND user_billing_states.subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE user_billing_states.grace_until
           END,
           updated_at = now()
    RETURNING *
), user_update AS (
    UPDATE users
       SET plan = sqlc.arg(plan)::user_plan,
           updated_at = now()
     WHERE id = sqlc.arg(user_id)::bigint
    RETURNING id
)
SELECT * FROM state;

-- name: MarkUserPastDue :one
UPDATE user_billing_states
   SET subscription_status = 'past_due',
       past_due_at = COALESCE(past_due_at, now()),
       locked_at = now(),
       lock_reason = 'past_due',
       grace_until = sqlc.narg(grace_until)::timestamptz,
       last_webhook_event_id = sqlc.arg(last_webhook_event_id)::text,
       updated_at = now()
 WHERE user_id = sqlc.arg(user_id)::bigint
RETURNING *;

-- name: MarkUserPaymentSucceeded :one
WITH state AS (
    UPDATE user_billing_states
       SET plan = CASE
               WHEN subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN 'pro'
               ELSE plan
           END,
           subscription_status = CASE
               WHEN subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN 'active'
               ELSE subscription_status
           END,
           past_due_at = CASE
               WHEN subscription_status IN ('past_due', 'unpaid', 'incomplete') THEN NULL
               ELSE past_due_at
           END,
           locked_at = NULL,
           lock_reason = NULL,
           grace_until = NULL,
           last_webhook_event_id = sqlc.arg(last_webhook_event_id)::text,
           updated_at = now()
     WHERE user_id = sqlc.arg(user_id)::bigint
    RETURNING *
), user_update AS (
    UPDATE users
       SET plan = state.plan,
           updated_at = now()
      FROM state
     WHERE users.id = state.user_id
    RETURNING users.id
)
SELECT * FROM state;

-- name: MarkUserCanceled :one
WITH state AS (
    UPDATE user_billing_states
       SET plan = 'free',
           subscription_status = 'canceled',
           canceled_at = COALESCE(canceled_at, now()),
           locked_at = now(),
           lock_reason = 'canceled',
           grace_until = NULL,
           cancel_at_period_end = false,
           last_webhook_event_id = sqlc.arg(last_webhook_event_id)::text,
           updated_at = now()
     WHERE user_id = sqlc.arg(user_id)::bigint
    RETURNING *
), user_update AS (
    UPDATE users
       SET plan = 'free',
           updated_at = now()
     WHERE id = sqlc.arg(user_id)::bigint
    RETURNING id
)
SELECT * FROM state;

-- name: ClearUserBillingLock :one
WITH state AS (
    UPDATE user_billing_states
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
     WHERE user_id = $1
    RETURNING *
), user_update AS (
    UPDATE users
       SET plan = state.plan,
           updated_at = now()
      FROM state
     WHERE users.id = state.user_id
    RETURNING users.id
)
SELECT * FROM state;

-- ─── org_usage_counters ────────────────────────────────────────────

-- name: GetOrgUsageCounters :one
SELECT * FROM org_usage_counters WHERE org_id = $1;

-- name: UpsertOrgUsageCounters :one
INSERT INTO org_usage_counters (
    org_id,
    repo_storage_bytes,
    object_storage_bytes,
    actions_log_bytes,
    actions_artifact_bytes,
    actions_minutes_used,
    actions_period_start,
    actions_period_end,
    calculated_at
)
VALUES (
    sqlc.arg(org_id)::bigint,
    sqlc.arg(repo_storage_bytes)::bigint,
    sqlc.arg(object_storage_bytes)::bigint,
    sqlc.arg(actions_log_bytes)::bigint,
    sqlc.arg(actions_artifact_bytes)::bigint,
    sqlc.arg(actions_minutes_used)::bigint,
    sqlc.arg(actions_period_start)::timestamptz,
    sqlc.arg(actions_period_end)::timestamptz,
    COALESCE(sqlc.narg(calculated_at)::timestamptz, now())
)
ON CONFLICT (org_id) DO UPDATE
   SET repo_storage_bytes = EXCLUDED.repo_storage_bytes,
       object_storage_bytes = EXCLUDED.object_storage_bytes,
       actions_log_bytes = EXCLUDED.actions_log_bytes,
       actions_artifact_bytes = EXCLUDED.actions_artifact_bytes,
       actions_minutes_used = EXCLUDED.actions_minutes_used,
       actions_period_start = EXCLUDED.actions_period_start,
       actions_period_end = EXCLUDED.actions_period_end,
       calculated_at = EXCLUDED.calculated_at,
       updated_at = now()
RETURNING *;

-- name: RecalculateOrgUsageCounters :one
WITH repo_usage AS (
    SELECT COALESCE(sum(disk_used_bytes), 0)::bigint AS repo_storage_bytes
    FROM repos
    WHERE owner_org_id = sqlc.arg(org_id)::bigint
      AND deleted_at IS NULL
),
action_usage AS (
    SELECT
        COALESCE(sum(s.log_byte_count), 0)::bigint AS actions_log_bytes
    FROM workflow_runs r
    JOIN repos repo ON repo.id = r.repo_id
    JOIN workflow_jobs j ON j.run_id = r.id
    JOIN workflow_steps s ON s.job_id = j.id
    WHERE repo.owner_org_id = sqlc.arg(org_id)::bigint
),
actions_minutes AS (
    SELECT COALESCE(sum(
        CASE
            WHEN j.status IN ('completed', 'cancelled')
             AND j.started_at IS NOT NULL
             AND j.completed_at IS NOT NULL
             AND j.completed_at >= sqlc.arg(actions_period_start)::timestamptz
             AND j.completed_at < sqlc.arg(actions_period_end)::timestamptz
            THEN CEIL(EXTRACT(EPOCH FROM (j.completed_at - j.started_at)) / 60.0)::bigint
            ELSE 0
        END
    ), 0)::bigint AS actions_minutes_used
    FROM workflow_jobs j
    JOIN workflow_runs r ON r.id = j.run_id
    JOIN repos repo ON repo.id = r.repo_id
    WHERE repo.owner_org_id = sqlc.arg(org_id)::bigint
),
artifact_usage AS (
    SELECT COALESCE(sum(a.byte_count), 0)::bigint AS actions_artifact_bytes
    FROM workflow_artifacts a
    JOIN workflow_runs r ON r.id = a.run_id
    JOIN repos repo ON repo.id = r.repo_id
    WHERE repo.owner_org_id = sqlc.arg(org_id)::bigint
),
upserted AS (
    INSERT INTO org_usage_counters (
        org_id,
        repo_storage_bytes,
        object_storage_bytes,
        actions_log_bytes,
        actions_artifact_bytes,
        actions_minutes_used,
        actions_period_start,
        actions_period_end,
        calculated_at
    )
    SELECT
        sqlc.arg(org_id)::bigint,
        repo_usage.repo_storage_bytes,
        action_usage.actions_log_bytes + artifact_usage.actions_artifact_bytes,
        action_usage.actions_log_bytes,
        artifact_usage.actions_artifact_bytes,
        actions_minutes.actions_minutes_used,
        sqlc.arg(actions_period_start)::timestamptz,
        sqlc.arg(actions_period_end)::timestamptz,
        now()
    FROM repo_usage, action_usage, actions_minutes, artifact_usage
    ON CONFLICT (org_id) DO UPDATE
       SET repo_storage_bytes = EXCLUDED.repo_storage_bytes,
           object_storage_bytes = EXCLUDED.object_storage_bytes,
           actions_log_bytes = EXCLUDED.actions_log_bytes,
           actions_artifact_bytes = EXCLUDED.actions_artifact_bytes,
           actions_minutes_used = EXCLUDED.actions_minutes_used,
           actions_period_start = EXCLUDED.actions_period_start,
           actions_period_end = EXCLUDED.actions_period_end,
           calculated_at = EXCLUDED.calculated_at,
           updated_at = now()
    RETURNING *
)
SELECT * FROM upserted;

-- name: ListActiveOrgIDsForUsageRecalc :many
SELECT id
FROM orgs
WHERE deleted_at IS NULL
ORDER BY id ASC
LIMIT sqlc.arg(lim)::integer;

-- name: CreateOrgUsageSnapshot :one
INSERT INTO org_usage_snapshots (
    org_id,
    source,
    repo_storage_bytes,
    object_storage_bytes,
    actions_log_bytes,
    actions_artifact_bytes,
    actions_minutes_used,
    actions_period_start,
    actions_period_end
)
SELECT
    org_id,
    sqlc.arg(source)::text,
    repo_storage_bytes,
    object_storage_bytes,
    actions_log_bytes,
    actions_artifact_bytes,
    actions_minutes_used,
    actions_period_start,
    actions_period_end
FROM org_usage_counters
WHERE org_id = sqlc.arg(org_id)::bigint
RETURNING *;

-- name: ListOrgUsageSnapshots :many
SELECT * FROM org_usage_snapshots
WHERE org_id = $1
ORDER BY captured_at DESC, id DESC
LIMIT $2;

-- ─── org_quota_overrides ───────────────────────────────────────────

-- name: ListOrgQuotaOverrides :many
SELECT * FROM org_quota_overrides
WHERE org_id = $1
ORDER BY kind ASC;

-- name: GetOrgQuotaOverride :one
SELECT * FROM org_quota_overrides
WHERE org_id = $1
  AND kind = $2;

-- name: UpsertOrgQuotaOverride :one
INSERT INTO org_quota_overrides (
    org_id,
    kind,
    limit_value,
    unlimited,
    note,
    created_by_user_id
)
VALUES (
    sqlc.arg(org_id)::bigint,
    sqlc.arg(kind)::org_quota_kind,
    sqlc.narg(limit_value)::bigint,
    sqlc.arg(unlimited)::boolean,
    sqlc.arg(note)::text,
    sqlc.narg(created_by_user_id)::bigint
)
ON CONFLICT (org_id, kind) DO UPDATE
   SET limit_value = EXCLUDED.limit_value,
       unlimited = EXCLUDED.unlimited,
       note = EXCLUDED.note,
       created_by_user_id = EXCLUDED.created_by_user_id,
       updated_at = now()
RETURNING *;

-- name: DeleteOrgQuotaOverride :execrows
DELETE FROM org_quota_overrides
WHERE org_id = $1
  AND kind = $2;
