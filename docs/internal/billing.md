# Billing and paid organizations

shithub's first paid surface is organization billing. The code does not
ship billing yet; this document records the product and engineering
contract that the PAYMENTS sprint series implements.

The current implementation already has the important shape for paid
organizations: `orgs.plan` is an enum with `free`, `team`, and
`enterprise`; organizations own repositories; organization members and
teams exist; branch protection and PR review gates exist; Actions has
schema for org/repo secrets, variables, and artifacts. Billing must
turn that substrate into a fair hosted service without taxing
public/open-source collaboration.

## Product contract

As of 2026-05-12, GitHub's public pricing page presents Free at
`$0`, Team at `$4/user/month`, and Enterprise starting at
`$21/user/month`. shithub follows the same mental model but removes
Copilot/AI promises from the paid-org offering.

Initial decisions:

- Free organizations remain self-serve.
- Team is `$4` per active organization member per month.
- Active organization members, including owners, count as paid seats.
- Team has no launch trial.
- Enterprise is a visible contact-sales stub, not self-serve.
- Stripe Billing is the first payment processor.
- PayPal, manual invoices, SAML, SCIM, LDAP, enterprise account
  hierarchy, and contracts are deferred.
- Self-serve organization creation should present plan selection first
  when billing is enabled; choosing Team creates the organization and
  then hands the owner into billing to finish checkout.

The fairness rule is explicit: public/open-source collaboration should
stay generous. Paid gates focus on private collaboration, hosted cost,
advanced organization controls, and support expectations.

## Pricing copy rules

Pricing and onboarding pages must describe only features shithub can
actually deliver on the hosted service. Before changing pricing copy,
refresh the official GitHub pricing source and the Stripe Billing docs
because both are time-sensitive inputs.

Rules for paid-org copy:

- Do not mention Copilot, AI agents, AI code review, or AI quotas.
- Do not promise SAML, SCIM, LDAP, managed users, audit exports, data
  residency, compliance attestations, contracts, or custom support
  until the matching implementation sprint ships.
- Do not advertise Packages, Pages, Wikis, Projects, Actions minutes,
  or storage quotas until those surfaces have enforcement and usage
  accounting.
- Use upgrade language for unavailable Team features instead of hiding
  existing data. Downgrades preserve configuration and make gated
  settings read-only where possible.
- Keep public/open-source collaboration generous in both copy and
  enforcement. Avoid copy that makes public repositories feel like a
  second-class Free tier.
- Enterprise is a contact-sales stub in v1. It should collect interest
  without promising contractual features.

## Entitlement matrix

| Capability | Free | Team | Enterprise stub |
| --- | --- | --- | --- |
| Public org repositories | Included | Included | Contact sales |
| Basic private org repositories | Included | Included | Contact sales |
| Org members and invitations | Included | Billed by active member | Contact sales |
| Visible teams | Included | Included | Contact sales |
| Secret teams | Upgrade | Included | Contact sales |
| Basic branch protection | Included | Included | Contact sales |
| Advanced private-repo branch protection | Upgrade | Included | Contact sales |
| Required reviewers on private org repos | Upgrade | Included | Contact sales |
| CODEOWNERS review | Deferred | Deferred | Deferred |
| Org-level Actions secrets | Upgrade | Included | Contact sales |
| Org-level Actions variables | Upgrade | Included | Contact sales |
| Actions minutes | Low quota once metered | Higher quota once metered | Contact sales |
| Actions artifacts/storage | Low quota once metered | Higher quota once metered | Contact sales |
| Packages storage | Deferred until Packages is active | Deferred until Packages is active | Deferred |
| Pages/Wikis/Projects | Do not promise until shipped | Do not promise until shipped | Deferred |
| Audit log export | Deferred | Deferred | Later Enterprise feature |
| SAML/SCIM/managed users | Deferred | Deferred | Later Enterprise feature |
| Data residency/compliance | Deferred | Deferred | Later Enterprise feature |
| Billing support | Basic instance support | Billing support after runbook exists | Contact sales |

## Current capability audit

Already present and safe to gate:

- Organizations with `plan` and `billing_email`.
- Organization members, owner role, and invitations.
- Teams, including `privacy='secret'`.
- Branch protection rules and required review counts.
- PR review and reviewer-request substrate.
- Org/repo Actions secrets and variables schema.

Present but missing enforcement or metering:

- Storage quota type exists, but quota persistence and enforcement are
  incomplete.
- Actions minutes, artifacts, and object usage need accounting before
  paid limits can be promised.
- Packages storage cannot be sold until the Packages sprint is active
  and quota enforcement exists.

Deferred:

- SAML, SCIM, LDAP, enterprise account hierarchy, audit-log export, data
  residency, compliance promises, and custom support SLAs.
- Copilot/AI features are intentionally outside shithub's paid-org
  product.

## Billing architecture

Stripe is the payment source of truth. shithub is the entitlement source
of truth.

The billing implementation should add a local billing domain that stores
only Stripe IDs and payment summaries, never card data. Webhooks update
local subscription state after signature verification. Policy and
request handlers read local billing/entitlement state and must not call
Stripe in hot paths.

Required local concepts:

- Stripe customer per billable organization.
- Subscription state per organization.
- Subscription item ID for seat quantity sync.
- Immutable webhook receipts with unique provider event IDs.
- Invoice/payment summaries for UI.
- Seat snapshots for auditability.
- Billing grace/lock state derived from processed subscription events.

PAYMENTS SP02 adds these as local database tables:

- `org_billing_states` stores the organization billing projection used
  by entitlement checks.
- `billing_seat_snapshots` records active and billable seat counts over
  time.
- `billing_invoices` stores invoice/payment summaries for billing UI.
- `billing_webhook_events` stores immutable provider event receipts for
  idempotent webhook processing.

New organizations receive a Free billing state from a database trigger,
and the migration backfills existing organizations as Free. Subscription
snapshot writes also keep `orgs.plan` synchronized as the
human-facing summary.

PAYMENTS SP03 adds the first Stripe operator contract:

- `billing.enabled=false` keeps paid-org flows disabled while retaining
  the local billing tables.
- `billing.stripe.secret_key`, `billing.stripe.webhook_secret`, and
  `billing.stripe.team_price_id` are required before Stripe routes are
  mounted.
- Checkout success, Checkout cancel, and Billing Portal return URLs may
  be overridden explicitly; otherwise the web layer derives absolute
  organization URLs from `auth.base_url`.
- `billing.grace_period` controls how long failed-payment states may
  remain unlocked before paid entitlements are cut off.

## Entitlement architecture

Paid feature checks must live behind a central entitlement package, not
as scattered `orgs.plan` checks in handlers.

`make lint-org-plan` enforces this boundary. Schema/sqlc plumbing may
store and scan the plan value, but product behavior should ask the
entitlement package whether a feature key is available.

Expected feature keys:

- `org.secret_teams`
- `org.advanced_branch_protection`
- `org.required_reviewers`
- `org.actions_org_secrets`
- `org.actions_org_variables`
- `org.private_collaboration_limit`
- `org.storage_quota`
- `org.actions_minutes_quota`

Authorization and entitlement are separate gates. A user must have both
the policy permission and the paid entitlement for gated writes. Denials
must preserve existing `policy.Maybe404` behavior where existence leaks
matter.

## Downgrade behavior

Downgrades must preserve customer data. Moving from Team to Free should
not delete teams, secrets, variables, branch rules, or review settings.
Existing gated resources become read-only where possible. Users can
remove gated configuration, but cannot create or expand it until the
organization upgrades again.

## Open questions for implementation

- Whether Free should limit private org collaborators before usage
  metering exists, or whether the first paid gates are advanced controls
  only.
- Whether required reviewers are gated only for private org repos. The
  current lean is private-org-only.
- Whether org-level Actions secrets and variables should be Team-only
  even for public repositories. The current lean is yes for org scope.
- Exact Free and Team quota numbers for Actions and storage. These must
  come from real host-cost estimates before SP08.

## Source references

- GitHub pricing: `https://github.com/pricing`
- GitHub plans docs:
  `https://docs.github.com/en/get-started/learning-about-github/githubs-plans`
- Stripe Billing: `https://docs.stripe.com/billing`
- Stripe pricing models:
  `https://docs.stripe.com/products-prices/pricing-models`
