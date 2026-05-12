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
continues to derive from the latest local billing projection. Handle any
Stripe-side cancellations or refunds in the Stripe dashboard until the
admin billing tools exist.
