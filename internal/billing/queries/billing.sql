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
