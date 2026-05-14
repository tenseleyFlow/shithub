# Stripe Billing

Operator runbook for turning on shithub's hosted paid flows: Team
organizations and optional Pro personal accounts. Pair this with
[`../billing.md`](../billing.md) for the product contract and
entitlement matrix.

## Preconditions

- A verified Stripe account with payouts enabled to a bank account the
  operator controls.
- A recurring per-seat Product/Price for Team organizations. The v1
  price is `$4` USD per licensed organization seat per month.
- Optional recurring single-seat Product/Price for Pro accounts.
- A billing support email that appears in public policy pages.
- A documented refund/cancellation policy and privacy notice for Stripe
  payment processing data.
- Production `auth.base_url` points at the public HTTPS origin.
- Web and worker processes share the same billing env vars.
- Database migrations are current.

Do not enable live billing before Stripe account identity, tax, payout,
and statement descriptor setup are complete. shithub stores only Stripe
IDs and derived payment summaries; card data stays in Stripe.

## Launch readiness checklist

Complete this checklist before flipping live-mode billing:

- Stripe account identity verified.
- Bank payout destination added and payout schedule visible.
- Statement descriptor reviewed.
- Support email can receive and reply to billing tickets.
- Public billing terms, refund/cancellation policy, and payment-data
  privacy language published.
- Stripe Tax decision recorded:
  - disabled and operator will handle tax outside shithub, or
  - enabled in Stripe and `SHITHUB_BILLING__STRIPE__AUTOMATIC_TAX=true`.
- Test-mode Team checkout completes and webhook activates the org.
- Test-mode Pro checkout completes if `pro_price_id` is configured.
- Test-mode webhook replay has been drilled from Stripe Dashboard.
- Billing alert rules are loaded in Prometheus/Grafana and firing paths
  are tested with a synthetic failure.

## Stripe setup

### Team price

1. Create a Product named `shithub Team`.
2. Create a recurring monthly Price:
   - Billing scheme: per unit.
   - Currency: USD.
   - Unit amount: `400`.
   - Usage: licensed quantity.
3. Copy the Price ID, for example `price_...`.

This Price must bill by quantity. shithub creates Team Checkout
Sessions with `Quantity=<active org members>` and later updates the
Stripe subscription item quantity from the seat-sync worker. If the
Dashboard Price was created as a fixed flat-rate subscription, extra
seats will not bill correctly. Stripe Prices are immutable in the
fields that matter here; create a new Team Price and replace
`SHITHUB_BILLING__STRIPE__TEAM_PRICE_ID`.

Do not let customers edit Team quantity in the Billing Portal. shithub
owns Team quantity through organization membership, so Dashboard/Portal
quantity edits create subscription drift.

### Pro price

Create a separate recurring monthly Price for `shithub Pro`:

- Billing scheme: flat/single unit is acceptable.
- Currency: USD.
- Unit amount: `400`.
- Usage: licensed quantity with quantity `1`.

Copy this Price ID to `SHITHUB_BILLING__STRIPE__PRO_PRICE_ID`. When the
field is empty, `/settings/billing` renders Pro as unavailable and
personal-account upgrades cannot start.

### API key and webhooks

1. Create a restricted secret key if possible. It must be able to:
   - create/read Customers,
   - create Checkout Sessions,
   - create Billing Portal Sessions,
   - update Subscription Items,
   - read invoice/subscription objects delivered by webhooks.
2. Create a webhook endpoint for:
   - `checkout.session.completed`
   - `customer.subscription.created`
   - `customer.subscription.updated`
   - `customer.subscription.deleted`
   - `invoice.finalized`
   - `invoice.payment_succeeded`
   - `invoice.payment_failed`
   - `invoice.voided`
   - `invoice.marked_uncollectible`
   - `charge.refunded`
3. Point the endpoint at:
   `https://<host>/stripe/webhook`
4. Copy the webhook signing secret, for example `whsec_...`.

Enable Stripe Tax only after the Stripe account is configured for it.
If it is enabled in shithub while Stripe is not ready, Checkout can fail
before the user reaches payment.

## Enable test mode

Use Stripe test-mode keys first:

```sh
SHITHUB_BILLING__ENABLED=true
SHITHUB_BILLING__GRACE_PERIOD=336h
SHITHUB_BILLING__STRIPE__SECRET_KEY=sk_test_...
SHITHUB_BILLING__STRIPE__WEBHOOK_SECRET=whsec_...
SHITHUB_BILLING__STRIPE__TEAM_PRICE_ID=price_...
# Optional: enable Pro checkout after creating the Pro Price.
SHITHUB_BILLING__STRIPE__PRO_PRICE_ID=price_...
SHITHUB_BILLING__STRIPE__AUTOMATIC_TAX=false
SHITHUB_BILLING__ENFORCE__USER_REQUIRED_REVIEWERS=false
SHITHUB_BILLING__ENFORCE__USER_ADVANCED_BRANCH_PROTECTION=false
SHITHUB_BILLING__ENFORCE__USER_PROFILE_PINS_BEYOND_FREE=false
```

Then validate and restart:

```sh
shithubd config validate
systemctl restart shithubd-web shithubd-worker
```

If `config validate` fails, fix the named key before restarting. The
web process mounts billing routes only when billing is configured; the
worker records local seat usage. Stripe quantity changes are explicit
licensed-seat operations; membership changes must not silently change
the subscription quantity.

## Smoke test

1. Sign in as an owner.
2. Visit `/organizations/plan`.
3. Confirm the plan picker appears.
4. Choose Team, create a test organization, and confirm redirect to
   hosted Stripe Checkout.
5. Complete Stripe Checkout with a test card.
6. Confirm Stripe redirects back to shithub.
7. Confirm `/organizations/<org>/settings/billing` eventually shows:
   - current plan: Team,
   - subscription: Active,
   - payment source: Stripe customer configured,
   - licensed, used, and available seat counts.
8. Invite or remove a member and verify the worker records local used
   seat changes without changing the Stripe subscription quantity.
9. If Pro is configured, visit `/settings/billing`, start checkout,
   complete it with a test card, and confirm the user plan becomes Pro
   after webhook processing.
10. Open the Stripe Billing Portal from both paid settings pages and
    confirm customers can change payment method or cancel, but cannot
    edit Team subscription quantity directly.

If the UI remains Free after checkout, inspect webhook receipts and the
web journal. The most likely causes are a wrong webhook secret, missing
event subscription, or Stripe delivering events to the wrong host.

## Go live

1. Replace test-mode values with live-mode Stripe keys and Price ID.
2. Re-run `shithubd config validate`.
3. Restart web and worker.
4. Create a low-risk real organization and complete checkout.
5. Confirm a live invoice appears in Stripe and shithub.
6. Confirm payouts are enabled in Stripe and the first payout schedule
   is visible.

Do not promise Enterprise, SAML, SCIM, compliance, or custom support
from the billing UI until the matching product and operational support
exist.

## Incident checks

| Symptom | First check |
| --- | --- |
| Plan picker missing | `billing.enabled` false or web not restarted. |
| Checkout button 404s | billing routes were not mounted; validate Stripe config and restart web. |
| Checkout creation fails | secret key, Team Price ID, Stripe Tax, or network reachability. |
| Webhook returns 400 | wrong `billing.stripe.webhook_secret` or non-Stripe request. |
| Subscription stays Free | missing subscription webhook events or unmapped customer/subscription metadata. |
| Seat count stale | worker not running, seat-sync jobs failing, or Stripe key lacks Subscription Item update permission. |
| Billing portal unavailable | the organization has no Stripe customer yet; start Checkout first. |

## Billing alerts

Prometheus rules live in
`deploy/monitoring/prometheus/rules.yml` under `shithubd-billing`.
They cover:

- webhook failure rate,
- webhook backlog and failed receipts,
- Checkout creation failures,
- Team seat drift,
- past-due principals,
- quota overages.

Dashboard queries:

```promql
sum(rate(shithub_billing_webhook_events_total[15m])) by (event_type, result)
shithub_billing_webhook_backlog
sum(increase(shithub_billing_checkout_sessions_total{result="failure"}[10m])) by (subject_kind)
shithub_billing_org_seat_drift
shithub_billing_past_due_principals
shithub_billing_quota_overage_orgs
```

If these metrics are missing entirely, confirm the web process is on a
build with SP10, `/metrics` is enabled, and Alloy/Prometheus is scraping
the `shithubd-web` target.

## Failed webhook replay

Use this when `BillingWebhookFailedReceipt` or
`BillingWebhookBacklogHigh` fires.

1. Inspect the latest failed receipts:

   ```sql
   SELECT provider_event_id, event_type, received_at, processing_attempts,
          process_error, subject_kind, subject_id
     FROM billing_webhook_events
    WHERE processed_at IS NULL
    ORDER BY received_at DESC
    LIMIT 50;
   ```

2. Fix the root cause first:
   - wrong webhook secret,
   - missing event type in Stripe endpoint,
   - price-kind mismatch,
   - duplicate subscription for the same customer,
   - stale or manually edited Stripe state.
3. In Stripe Dashboard, open the webhook endpoint, select the event,
   and click **Resend**.
4. Confirm the receipt becomes processed:

   ```sql
   SELECT provider_event_id, processed_at, process_error
     FROM billing_webhook_events
    WHERE provider_event_id = '<evt_...>';
   ```

The handler is idempotent and serialized by event id. Replaying a
successfully processed event should return 200 and leave local state
unchanged.

## Checkout failures

Use this when `BillingCheckoutFailures` fires.

1. Tail web logs for `org billing: create checkout` or
   `user billing: create checkout`.
2. Check `shithubd config print` for redacted but present billing keys.
3. Verify `auth.base_url` is the public HTTPS origin.
4. Verify the Team Price ID is live/test-mode matched to the secret key.
5. Verify the Team Price is per-unit/licensed. A fixed flat-rate Price
   can create checkout, but it will not bill extra seats correctly.
6. If `automatic_tax=true`, confirm Stripe Tax is configured in the
   matching Stripe account mode.

## Subscription drift repair

Use this when `BillingSeatDrift` fires or an org owner reports wrong
seat counts.

1. Identify drifting organizations:

   ```sql
   WITH seat_counts AS (
     SELECT org_id, count(*) AS seats
       FROM org_members
      GROUP BY org_id
   )
   SELECT s.org_id, s.stripe_subscription_id, s.stripe_subscription_item_id,
          s.licensed_seats,
          s.used_seats AS local_used_seats,
          COALESCE(c.seats, 0) AS actual_used_seats
     FROM org_billing_states s
     LEFT JOIN seat_counts c ON c.org_id = s.org_id
    WHERE s.plan = 'team'
      AND (
        s.used_seats <> COALESCE(c.seats, 0)
        OR s.licensed_seats < s.used_seats
      );
   ```

2. Enqueue or run `org:billing_seat_sync` for the org to refresh local
   used-seat state.
3. If `licensed_seats < actual_used_seats`, do not silently change
   Stripe from the worker. Have an owner add seats through the explicit
   seat-management flow once SP16 is live, or perform an operator repair
   that updates both Stripe quantity and local licensed-seat state.
4. Audit manual repairs:

   ```sql
   INSERT INTO auth_audit_log (actor_id, action, target_type, target_id, meta)
   VALUES (<operator_user_id>, 'billing.manual_seat_repair', 'org', <org_id>,
           jsonb_build_object('stripe_subscription_item_id', '<si_...>',
                              'quantity', <actual_seats>,
                              'reason', '<ticket/url>'));
   ```

## Manual downgrade or upgrade

Prefer Stripe Dashboard changes and webhooks over direct SQL. Direct SQL
is only for Stripe outages or data-repair incidents.

- Upgrade: create or fix the subscription in Stripe, ensure metadata has
  `shithub_subject_kind` and `shithub_subject_id`, then resend the
  latest `customer.subscription.updated` event.
- Downgrade: cancel the subscription in Stripe, then resend
  `customer.subscription.deleted`.
- Emergency local lock: set the local subscription status to a
  payment-action-needed state only after recording an audit row and a
  support ticket.

Every manual operation needs a matching `auth_audit_log` row with the
operator user id, target type (`org` or `user`), target id, Stripe object
ids, and ticket/reason.

## Refund handling

Issue refunds in Stripe Dashboard. shithub records the refund when
Stripe sends `charge.refunded`.

1. Refund the charge or invoice in Stripe.
2. Do not assume refund means cancellation. If future billing should
   stop, cancel the subscription too.
3. Confirm shithub recorded the refund:

   ```sql
   SELECT stripe_invoice_id, status, refunded_at
     FROM billing_invoices
    WHERE stripe_invoice_id = '<in_...>';
   ```

4. Audit the support action:

   ```sql
   INSERT INTO auth_audit_log (actor_id, action, target_type, target_id, meta)
   VALUES (<operator_user_id>, 'billing.refund_issued', '<org|user>', <id>,
           jsonb_build_object('stripe_invoice_id', '<in_...>',
                              'reason', '<ticket/url>'));
   ```

## Past-due accounts

Use this when `BillingPastDuePrincipals` fires.

1. Find accounts needing action:

   ```sql
   SELECT 'org' AS kind, org_id AS id, plan::text, subscription_status::text,
          past_due_at, grace_until
     FROM org_billing_states
    WHERE subscription_status IN ('past_due', 'unpaid', 'incomplete')
   UNION ALL
   SELECT 'user' AS kind, user_id AS id, plan::text, subscription_status::text,
          past_due_at, grace_until
     FROM user_billing_states
    WHERE subscription_status IN ('past_due', 'unpaid', 'incomplete')
    ORDER BY past_due_at NULLS LAST;
   ```

2. Confirm Stripe agrees with local state.
3. If payment was recovered, resend the latest subscription update or
   invoice payment event.
4. If payment failed permanently, let the grace window expire and let
   entitlements move features to billing-action-needed.

## Quota overages

Use this when `BillingQuotaOverage` fires.

1. Open the org billing settings page as a site admin. The Usage panel
   shows storage and Actions minutes against the effective limits.
2. If counters look stale, run `org:usage_recalc` for that org.
3. If the overage is legitimate, ask the org owner to upgrade or reduce
   usage. For support incidents, add a temporary quota override in the
   site-admin debug panel; it is attributed to the operator.
4. Do not edit `org_usage_counters` directly unless repairing corrupted
   metering after a bug. If you do, create an `auth_audit_log` row.

## Billing outage response

If Stripe is unreachable or webhooks are delayed:

1. Check Stripe status and local network errors.
2. Leave `billing.enabled=true` if existing customers can still use the
   site; disabling billing hides checkout/portal routes and can make
   support harder.
3. Pause public paid launch links if new checkouts are failing.
4. Keep workers running so delayed webhooks and seat-sync jobs catch up.
5. After recovery, inspect failed webhook receipts, replay them, and
   verify `shithub_billing_webhook_backlog{state="pending"} == 0`.

## Rollback

To pause paid onboarding without changing stored subscription state:

1. Set `SHITHUB_BILLING__ENABLED=false`.
2. Restart web and worker.

Existing local billing rows remain in the database. Billing routes
unmount, plan comparison links become disabled, and entitlement state
continues to derive from the latest local billing projection.

## Subject resolution chain (PRO08)

When a Stripe event arrives, the webhook handler walks this chain to
identify which shithub subject (org or user) the event applies to.
The first match wins; events that fall off the end are loud-failed
(except `customer.subscription.deleted`, which is a 200 no-op so
Stripe stops retrying — operator reconciles manually).

| # | Source | Applies to | Notes |
|---|---|---|---|
| 1 | `metadata.shithub_subject_kind` + `shithub_subject_id` | checkout, subscription | PRO04 — only path that resolves user-kind via metadata |
| 2 | `metadata.shithub_org_id` | checkout, subscription | Legacy SP03 — org-only backstop for pre-PRO04 customers |
| 3 | `client_reference_id` | checkout only | Legacy SP03 — parsed as int, org-only by convention |
| 4 | `customer.id` lookup against both `org_billing_states` and `user_billing_states` | all event types | User table searched first then org |
| 5 | `subscription.id` lookup against both tables | subscription, invoice | Used when customer lookup misses |

Invoice events do not check metadata (1–3); they go straight to
customer/subscription lookup. Stripe doesn't stamp our metadata on
invoices by default — they inherit from the subscription via
`subscription_data.metadata` set at checkout creation time.

## Per-feature enforcement flags (PRO07 + PRO08)

User-tier paygates (Pro) ship in report-only mode by default. Each
feature has an independent operator flag that flips it from report-
only to hard enforce. The flag is one-way until the operator reverts
it — flip per feature after 7 days of clean telemetry.

| Config key | Gate site | Default |
|---|---|---|
| `SHITHUB_BILLING__ENFORCE__USER_REQUIRED_REVIEWERS` | branch-protection rule save | `false` |
| `SHITHUB_BILLING__ENFORCE__USER_ADVANCED_BRANCH_PROTECTION` | branch-protection rule save (prevent_*, signing, status checks) | `false` |
| `SHITHUB_BILLING__ENFORCE__USER_PROFILE_PINS_BEYOND_FREE` | profile pin save | `false` |

Report-only mode logs `entitlements.report_only_deny` events with
the principal + feature. Tail logs for 7 days, confirm no Free user
is tripping a gate, then flip the relevant flag and redeploy.

## Refunds (PRO08 D2)

Stripe refunds are issued from the Stripe Dashboard. shithub picks
up the `charge.refunded` event automatically and:

1. Looks up the invoice row by `stripe_invoice_id` (from `charge.invoice`).
2. Flips its status to `refunded` and stamps `refunded_at`.
3. Surfaces the refunded state on `/settings/billing` and the
   org billing settings page.

Refunds do **not** automatically cancel the subscription. If you
want to revoke Pro access alongside the refund, cancel the
subscription separately in the Stripe Dashboard — that fires
`customer.subscription.deleted` which shithub handles by setting
`users.plan='free'` (or org equivalent).

A refund for an invoice shithub has never seen (e.g., an out-of-band
one-off charge) logs a warning and 200-no-ops — investigate via
the inspection query below.

## Operator inspection: failed webhook events (PRO08 A2 + Agent A)

Webhook receipts that failed to apply (resolver missed, guard
refused, apply errored) are kept in `billing_webhook_events` with a
non-empty `process_error`. PRO08 added (subject_kind, subject_id)
columns so operators can answer "what did this event apply to" even
on failed rows.

To see events received but not yet successfully applied:

```sql
SELECT provider_event_id, event_type, received_at, processing_attempts,
       process_error, subject_kind, subject_id
  FROM billing_webhook_events
 WHERE process_error <> ''
    OR (processed_at IS NULL AND processing_attempts > 0)
 ORDER BY received_at DESC
 LIMIT 50;
```

The same data is available via `orgbilling.ListFailedWebhookEvents`
from Go code.

## Cross-kind misroute protection (PRO08 A1)

When both `STRIPE_TEAM_PRICE_ID` and `STRIPE_PRO_PRICE_ID` are
configured, the webhook guard refuses any subscription event whose
price-id doesn't match the resolved subject's expected price (Team
for orgs, Pro for users). A Pro-priced subscription with
metadata claiming `subject_kind=org` (or vice versa) hits the guard,
the apply is refused, and the receipt records the mismatch in
`process_error`. The guard also refuses subscription events with
empty `items.data` — otherwise an attacker who can spoof Stripe
could bypass the price check entirely.

## Stale events (PRO08 D4)

Stripe doesn't guarantee delivery order across retries. shithub
records the latest Stripe event timestamp per subject in
`{org,user}_billing_states.last_event_at` and refuses to apply
older events. Reverse-ordered retries (e.g., `subscription.updated[active]`
arriving after `subscription.updated[canceled]`) are dropped with
an `org billing: dropping stale Stripe event` log line and a
200-no-op to Stripe.

## Concurrent replay protection (PRO08 A3)

The webhook handler acquires a session-scoped advisory lock keyed
on the event id at request entry. Two concurrent deliveries of the
same event serialize at the lock; the racing replay returns 200
without running the apply. Production should never see this in
practice (Stripe doesn't fan-out parallel retries) — the lock
defends against malicious senders who hold the webhook secret.

## Subscription-overwrite guard (PRO08 D3)

If a customer somehow ends up with two active Stripe subscriptions
(operator manually created one in the Dashboard), shithub refuses
to flip its `stripe_subscription_id` to point at the new one.
The receipt records the mismatch — operator must reconcile the
Stripe-side state (cancel the duplicate) before the apply succeeds.
