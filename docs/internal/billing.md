# Billing and paid organizations

shithub's first paid surface is organization billing. This document
records the product and engineering contract that the PAYMENTS sprint
series implements.

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
- Self-serve organization creation presents `/organizations/plan` as
  the canonical plan selector. Choosing Team creates the organization
  and immediately redirects the owner to hosted Stripe Checkout.

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
| Effective private org collaborators | Up to 3 | Unlimited while active/in grace | Contact sales |
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

## Pro v1 user-tier matrix (PRO07)

Pro is the user-tier paid plan (single seat, $4/month). PRO01 ratified
the v1 feature set; PRO07 wires the enforcement matrix. CODEOWNERS is a
registered placeholder with a no-op enforce path until the parser ships.

| Capability | Free | Pro |
| --- | --- | --- |
| Public/private personal repos | Included | Included |
| Required reviewers on private personal repos | Upgrade | Included |
| Multi-reviewer (>1 approvals) on private personal repos | Upgrade | Included |
| Advanced branch protection on private personal repos | Upgrade | Included |
| CODEOWNERS review | Deferred | Deferred |
| Profile pins | 6 | 100 |

Multi-reviewer is **not** a separate feature constant — the numeric
threshold lives in the deny payload of `FeatureRequiredReviewers`.

### Per-feature enforcement flags

PRO05 plumbed user-kind report-only logging through every gating site.
PRO07 lights up the gates one feature at a time via
`billing.enforce.*` in the operator's config. Defaults are all false
(report-only). Each flag is a one-way deploy that operators can roll
back without code changes.

| Config key | Gate site | Default |
| --- | --- | --- |
| `billing.enforce.user_required_reviewers` | `internal/web/handlers/repo/settings_branches.go` | false |
| `billing.enforce.user_advanced_branch_protection` | `internal/web/handlers/repo/settings_branches.go` | false |
| `billing.enforce.user_profile_pins_beyond_free` | `internal/web/handlers/profile/pins.go` | false |

Rollout discipline:

1. Deploy with all flags false. Run the report-only telemetry query
   for 7 days. Confirm zero unexpected user-kind would-denies.
2. Flip one feature flag in staging. Soak 7 days.
3. Flip the same feature flag in production.
4. Repeat per feature.

PRO07's pitfall doc explicitly forbids enforcing a feature without an
unenforce path. New gating sites land with their own flag; do not
share flags across features.

### Downgrade preservation

`users.plan = 'free'` after cancellation grandfather's existing gated
state — required-reviewer rules, profile pins above 6, advanced flags
on existing rules. The gate refuses to **create** new gated state on
Free, but never deletes prior configuration. This is the same
contract as the org-tier downgrade.

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
- When billing routes are mounted, `/settings/organizations` links
  owner-managed organizations to their billing page's plan comparison.
  When billing is disabled, that action stays visibly unavailable
  instead of linking to an unmounted route.

The operator enablement flow is documented in
[`runbooks/stripe-billing.md`](./runbooks/stripe-billing.md).

PAYMENTS SP04 adds the self-serve onboarding flow:

- `/organizations/plan` is the canonical plan picker.
- Free setup creates the organization locally without Stripe.
- Team setup creates the organization, creates or reuses the Stripe
  customer, counts billable seats, and redirects directly to hosted
  Stripe Checkout.
- Checkout success and cancel returns render shithub pages. Success
  tells the owner that activation waits for webhook processing; cancel
  keeps the organization on Free and offers a retry path.

PAYMENTS SP05 adds the local entitlement boundary. Product code must ask
`internal/entitlements` for feature decisions instead of inspecting
`orgs.plan` directly. The package derives access from
`org_billing_states`, understands billing-good-standing states, and
returns upgrade metadata for user-facing handlers.

PAYMENTS SP06 wires the first Team gates:

- Secret teams require Team to create. Existing secret teams remain
  visible to authorized viewers after downgrade; owners can remove
  members and repository grants, but adding members or granting more
  repository access is blocked until Team billing is active again.
- Required reviewers and advanced status-check branch protection are
  Team-only for private organization repositories. Public organization
  repositories keep those safety controls available on Free.
- Downgraded private organization repositories may delete protection
  rules or submit a rule update that clears the gated review/check
  settings.
- Org-level Actions secrets and variables require Team for create or
  update in both HTML settings and REST API routes. Delete stays
  available so owners can clean up gated configuration after downgrade.
- Org-level Actions secrets and variables API routes require
  organization owner or site-admin access before entitlement checks.

PAYMENTS SP07 completes the first self-serve billing settings surface:

- `GET /organizations/{org}/settings/billing` is owner-only and is
  linked from organization settings when Stripe Billing is configured.
- The page shows current local plan state, subscription status, payment
  source summary, recent Stripe-synced invoice snapshots, and actionable
  banners for past-due, grace-period, canceled, scheduled-cancel, and
  billing-action-needed states.
- Seat accounting is shown as three separate values: current active
  members, billable seats from the latest local billing state, and
  pending invitations. Pending invitations are explicitly not billed
  until accepted.
- Team organizations manage payment method, invoices, cancellation, and
  downgrade through Stripe Billing Portal. shithub never collects card
  data directly and downgrades continue to preserve paid configuration
  as read-only data.
- Normal organization owners do not see raw Stripe customer,
  subscription, or subscription-item IDs. Site admins see a debug panel
  with those IDs and the latest locally recorded webhook receipt state.

PAYMENTS SP06a adds the first private-collaboration limit:

- Free organizations may have up to 3 unique humans with effective
  access to at least one private organization repository.
- Team organizations with active, trialing, or in-grace subscriptions
  have unlimited private collaborators.
- The effective private-collaborator set counts org owners, direct
  collaborators on private org repos, and team members who inherit a
  private repo grant through direct team membership or one-level parent
  team inheritance. Plain org members do not count unless they gain
  private repo access through one of those paths.
- Public repository collaboration never counts toward the limit.
- Downgrades preserve existing access even when the org is already over
  the Free limit, but writes that add a new effective private
  collaborator are blocked until the org upgrades or removes access.
- Creating/importing a private org repository and changing an org repo
  from public to private are blocked on Free when the resulting private
  collaborator set would exceed the limit.
- Cleanup writes remain available: removing org members, team members,
  direct collaborators, team repo grants, and gated configuration must
  not require Team.

## Entitlement architecture

Paid feature checks must live behind a central entitlement package, not
as scattered `orgs.plan` checks in handlers.

`make lint-org-plan` enforces this boundary. Schema/sqlc plumbing may
store and scan the plan value, but product behavior should ask the
entitlement package whether a feature key is available.

The package entrypoint is `entitlements.ForOrg(ctx, deps, orgID)`,
which loads the local `org_billing_states` projection and returns a
request-scoped entitlement set. Callers then use `CanUse(feature)` for
feature decisions and `Limit(name)` for paid limit metadata. The legacy
`CheckOrgFeature` helper is a thin wrapper for handlers that need only
one feature. These calls are deterministic and never call Stripe.

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

Entitlement outcomes are:

- Free organizations receive `upgrade_required` for Team features.
- Team organizations with `active` or `trialing` subscriptions receive
  the feature.
- Team organizations in `past_due` remain usable only while their local
  grace window has not expired.
- Team organizations with incomplete, unpaid, paused, canceled, or
  expired-grace billing receive `billing_action_needed`.
- Enterprise remains a contact-sales stub and receives
  `enterprise_contact_sales` until a later Enterprise sprint defines a
  real feature set.

Handler upgrade helpers map paid-feature denials to HTTP 402 metadata
and a billing-settings path, but handlers must still perform normal
authorization first and preserve 404 masking for private resources.

## Downgrade behavior

Downgrades must preserve customer data. Moving from Team to Free should
not delete teams, secrets, variables, branch rules, or review settings.
Existing gated resources become read-only where possible. Users can
remove gated configuration, but cannot create or expand it until the
organization upgrades again.

## Open questions for implementation

- Exact Free and Team quota numbers for Actions and storage. These must
  come from real host-cost estimates before SP08.

## Source references

- GitHub pricing: `https://github.com/pricing`
- GitHub plans docs:
  `https://docs.github.com/en/get-started/learning-about-github/githubs-plans`
- Stripe Billing: `https://docs.stripe.com/billing`
- Stripe pricing models:
  `https://docs.stripe.com/products-prices/pricing-models`
