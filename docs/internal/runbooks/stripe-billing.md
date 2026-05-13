# Stripe Billing

Operator runbook for turning on shithub's paid organization flow.
Pair this with [`../billing.md`](../billing.md) for the product
contract and entitlement matrix.

## Preconditions

- A verified Stripe account with payouts enabled to a bank account the
  operator controls.
- A recurring per-seat Product/Price for Team organizations. The v1
  price is `$4` USD per active organization member per month.
- Production `auth.base_url` points at the public HTTPS origin.
- Web and worker processes share the same billing env vars.
- Database migrations are current.

Do not enable live billing before Stripe account identity, tax, payout,
and statement descriptor setup are complete. shithub stores only Stripe
IDs and derived payment summaries; card data stays in Stripe.

## Stripe setup

1. Create a Product named `shithub Team`.
2. Create a recurring monthly Price:
   - Billing scheme: per unit.
   - Currency: USD.
   - Unit amount: `400`.
   - Usage: licensed quantity.
3. Copy the Price ID, for example `price_...`.
4. Create a restricted secret key if possible. It must be able to:
   - create/read Customers,
   - create Checkout Sessions,
   - create Billing Portal Sessions,
   - update Subscription Items,
   - read invoice/subscription objects delivered by webhooks.
5. Create a webhook endpoint for:
   - `checkout.session.completed`
   - `customer.subscription.created`
   - `customer.subscription.updated`
   - `customer.subscription.deleted`
   - `invoice.finalized`
   - `invoice.payment_succeeded`
   - `invoice.payment_failed`
   - `invoice.voided`
6. Point the endpoint at:
   `https://<host>/stripe/webhook`
7. Copy the webhook signing secret, for example `whsec_...`.

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
SHITHUB_BILLING__STRIPE__AUTOMATIC_TAX=false
```

Then validate and restart:

```sh
shithubd config validate
systemctl restart shithubd-web shithubd-worker
```

If `config validate` fails, fix the named key before restarting. The
web process mounts billing routes only when billing is configured; the
worker uses the same Stripe credentials for seat quantity sync.

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
   - a billable member count matching the org membership.
8. Invite or remove a member and verify the worker updates the Stripe
   subscription item quantity.

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

