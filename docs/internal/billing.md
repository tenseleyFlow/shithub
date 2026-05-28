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
- Team is `$4` per licensed organization seat per month.
- Active organization members, including owners, consume licensed
  seats.
- Team keeps purchased capacity (`licensed_seats`) separate from
  current usage (`used_seats`) so owners can buy seats before inviting
  members and can remove only unassigned seats.
- Team has no launch trial.
- Enterprise is a visible contact-sales stub, not self-serve.
- Stripe Billing is the first payment processor.
- PayPal, manual invoices, SAML, SCIM, LDAP, enterprise account
  hierarchy, and contracts are deferred.
- Self-serve organization creation presents `/organizations/plan` as
  the canonical plan selector. Choosing Team collects a licensed-seat
  count, shows the monthly total, creates the organization, records the
  pending seat intent, and redirects the owner to hosted Stripe
  Checkout with that quantity.

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
- Do not advertise product surfaces until they exist. Generic
  repository Packages, Wikis, and repository Projects have shipped
  baseline surfaces; Pages and additional package ecosystems remain
  deferred. Codespaces must be shown as unavailable until the core
  hosted development environment product ships. Storage and Actions
  quota copy may appear on owner-only billing settings once usage
  accounting exists, but public pricing pages must not present them as
  broadly enforced until the matching hard-deny gates have shipped.
- Use upgrade language for unavailable Team features instead of hiding
  existing data. Downgrades preserve configuration and make gated
  settings read-only where possible.
- Public pricing compare rows may name planned GitHub-parity
  capabilities only when the row also exposes the owning sprint and a
  non-shipped state such as `Planned` or `Partially shipped`; the Team
  column must not say `Included` until the owning sprint has shipped
  the user-visible feature and enforcement.
- Keep public/open-source collaboration generous in both copy and
  enforcement. Avoid copy that makes public repositories feel like a
  second-class Free tier.
- Free organization members and invitations exist, but public pricing
  should not advertise unlimited Free membership as a headline
  differentiator. The paid pitch should center licensed seats for
  private organization collaboration and controls.
- Enterprise is a contact-sales stub in v1. It should collect interest
  without promising contractual features.

## Entitlement matrix

| Capability | Free | Team | Enterprise stub |
| --- | --- | --- | --- |
| Public org repositories | Included | Included | Contact sales |
| Basic private org repositories | Included | Included | Contact sales |
| Base org members and invitations (not a pricing headline) | Included | Billed by licensed seat | Contact sales |
| Effective private org collaborators | Up to 3 | Unlimited while active/in grace | Contact sales |
| Visible teams | Included | Included | Contact sales |
| Secret teams | Upgrade | Included | Contact sales |
| Basic branch protection | Included | Included | Contact sales |
| Advanced private-repo branch protection | Upgrade | Included | Contact sales |
| Required reviewers on private org repos | Upgrade | Included | Contact sales |
| Private-repo CODEOWNERS review | Upgrade | Included | Contact sales |
| Scheduled reminders on private org repos | Upgrade | Included | Contact sales |
| Repository projects on private org repos | Upgrade | Included | Contact sales |
| Wikis on private org repos | Upgrade | Included | Contact sales |
| Multiple issue/PR assignees on private org repos | Upgrade | Included | Contact sales |
| Org-level Actions secrets | Upgrade | Included | Contact sales |
| Org-level Actions variables | Upgrade | Included | Contact sales |
| Environment-scoped Actions secrets | Upgrade on private org repos | Settings UI, REST API, and runner injection included | Contact sales |
| Environment protection rules and deployment branches | Upgrade on private org repos | Branch policy, wait timers, reviewer approval, and prevent-self-review configurable and enforced | Contact sales |
| Actions minutes | 2,000 min/month | 3,000 min/month | Contact sales |
| Actions artifacts/storage | Counts against shared storage quota | Counts against shared storage quota | Contact sales |
| Codespaces | Not available | Not available; launch blocker until hosted development environments ship | Contact sales |
| Packages storage | 500 MiB shared org storage quota | 2 GiB shared org storage quota | Contact sales |
| Secret scanning and push protection | Public repositories | Supported pattern scanning, repo/org security views, allowlist, and pre-receive push rejection for private org repos | Contact sales |
| Provider notification | Not available | Not available | Contact sales |
| Validity checks | Not available | Not available | Contact sales |
| Code scanning and security campaigns | Public repositories | SARIF ingestion, repo/org code scanning views, and campaign grouping for private org repos | Contact sales |
| Repository security advisories | Public repositories and personal repos | Draft/publish/withdraw/archive advisory workflow for private org repos | Contact sales |
| Required organization 2FA | Included | Included | Contact sales |
| Role-based access control | Included | Included | Contact sales |
| Audit log | Baseline event capture | Baseline event capture; org-owner UI and CSV export | Contact sales |
| Audit log API | Deferred | Deferred | Later Enterprise feature |
| SBOMs | Public repositories and personal repos | SPDX JSON generation/storage for private org repos | Contact sales |
| Artifact attestations | Public repositories and personal repos | Store/download in-toto statements for private org repos | Contact sales |
| App-style integrations | Organization webhooks and external check runs | Organization webhooks, external check runs, and required checks for private org repositories | Contact sales |
| Status checks | Included through branch protection | Included through branch protection | Contact sales |
| Pre-receive hooks | Deferred | Deferred | Enterprise Server planning item |
| Pages | Deferred until static Pages hosting is active | Deferred until static Pages hosting is active | Deferred |
| SAML/SCIM/managed users | Deferred | Deferred | Later Enterprise feature |
| Data residency/compliance | Deferred | Deferred | Later Enterprise feature |
| Billing support | Basic instance support | Billing support via published contact | Contact sales |

## Pro v1 user-tier matrix (PRO07)

Pro is the user-tier paid plan (single seat, $4/month). PRO01 ratified
the v1 feature set; PRO07 wires the enforcement matrix. SP19 ships the
CODEOWNERS parser and private-repo review enforcement for Pro users.

| Capability | Free | Pro |
| --- | --- | --- |
| Public/private personal repos | Included | Included |
| Required reviewers on private personal repos | Upgrade | Included |
| Multi-reviewer (>1 approvals) on private personal repos | Upgrade | Included |
| Advanced branch protection on private personal repos | Upgrade | Included |
| Private-repo CODEOWNERS review | Upgrade | Included |
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
- CODEOWNERS parsing, automatic code-owner reviewer requests, and
  code-owner-required merge gates.
- Org/repo Actions secrets and variables schema.

Present but still moving toward full enforcement:

- SP08 adds durable organization usage counters, usage snapshots, and
  site-admin quota overrides for storage and Actions minutes.
- Org-owned Actions dispatch now hard-denies new runs when the current
  monthly usage recalculation shows the organization is at or over its
  effective Actions minutes quota.
- Org-owned git pushes now hard-deny in pre-receive when the pushed
  repo's actual on-disk size would put the organization over its
  effective storage quota.
- New object-backed write paths must add hard-deny checks before their
  bytes can be advertised as quota-enforced. Actions artifacts and
  generic repository package uploads already enforce the storage quota.
- Packages storage is active for generic repository package uploads.
  Additional GitHub Packages ecosystems are deferred until follow-up
  package protocol sprints.
- Repository security advisories have repo list/detail/create/edit/state
  flows, markdown sanitization, lifecycle events, and private-org Team
  gates. Published advisories with package metadata feed the local dependency
  alert catalog and resolve alerts again when withdrawn or archived.
  Per-advisory user/team disclosure collaborators can view nonpublished
  advisory details; collaborator roles are stored for audit/disclosure
  membership, not write delegation.
- Required organization 2FA has shipped as a baseline org security
  control. Org owners toggle it from
  `/organizations/{org}/settings/security`; session, PAT, smart HTTP
  git, SSH git, hooks, and Actions trigger actors all carry confirmed
  TOTP state into repository policy. Public reads stay public, while
  org members/collaborators without confirmed 2FA cannot read private
  org repositories or write organization repositories.
- SP29 pricing comparison rows split shipped baseline controls from
  explicit planned/deferred rows: RBAC and status checks are baseline
  shipped, audit event capture exists and owners can browse org/repo
  rows from `/organizations/{org}/settings/audit-log` and export the
  filtered scope as capped CSV, repository SBOMs generate/download SPDX
  JSON from stored dependency snapshots, repository artifact attestations
  store/download in-toto Statement JSON documents, and organization
  webhooks plus external check runs provide the first app-style
  integration surface. Full GitHub Apps installation/auth remains
  planned, and audit-log API/pre-receive hooks remain
  Enterprise/deferred placement.
- Codespaces are not implemented. S41 Actions runner workspaces are
  ephemeral CI execution directories and must not be represented as
  hosted development environments. PAYMENTS SP28 marks this as a
  launch blocker and S50 owns the real Codespaces campaign.

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
- Subscription item ID for licensed-seat quantity sync.
- Immutable webhook receipts with unique provider event IDs.
- Invoice/payment summaries for UI.
- Seat snapshots for auditability.
- Billing grace/lock state derived from processed subscription events.

PAYMENTS SP02 adds these as local database tables:

- `org_billing_states` stores the organization billing projection used
  by entitlement checks.
- `billing_seat_snapshots` records legacy active/billable counts and
  SP13's explicit licensed, used, and available seat counts over time.
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
- Team setup asks for a licensed-seat count, shows `$4 x N seats`, creates
  the organization, records a pending checkout seat snapshot, creates or
  reuses the Stripe customer, and redirects directly to hosted Stripe
  Checkout with `Quantity=N`.
- Checkout success and cancel returns render shithub pages. Success
  tells the owner that activation waits for webhook processing and shows
  the intended seat count; cancel keeps the organization on Free and
  offers a retry path with that same count.

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
- Seat accounting is shown as used seats, licensed seats, available
  seats, and pending invitations. Pending invitations are explicitly not
  billed until accepted.
- Team organizations manage payment method, invoices, cancellation, and
  downgrade through Stripe Billing Portal. shithub never collects card
  data directly and downgrades continue to preserve paid configuration
  as read-only data.
- Normal organization owners do not see raw Stripe customer,
  subscription, or subscription-item IDs. Site admins see a debug panel
  with those IDs, the latest locally recorded webhook receipt state,
  and support controls for saving or clearing storage and Actions
  minutes quota overrides.

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

PAYMENTS SP08 starts hosted-cost metering:

- Free organizations have a 500 MiB storage quota and 2,000 Actions
  minutes per calendar month.
- Team organizations in good standing have a 2 GiB storage quota and
  3,000 Actions minutes per calendar month.
- Storage usage is tracked as bare repository bytes plus tracked object
  bytes. The current recalculation source uses `repos.disk_used_bytes`,
  finalized Actions step log bytes, Actions artifact byte counts, and
  durable `repo_package_files.size_bytes` package file metadata; other
  object surfaces must be added as their storage metadata becomes
  durable.
- Actions minutes are counted from completed or canceled workflow job
  runtime, rounded up to the next whole minute, within the current
  monthly usage period. Metered runtime is capped at the job's declared
  `timeout_minutes` so stale terminal rows cannot bill past the maximum
  execution time the runner was allowed to consume.
- `org_usage_counters` stores the current projection,
  `org_usage_snapshots` records audit snapshots, and
  `org_quota_overrides` lets site admins temporarily override a quota
  for support cases.
- `org:usage_recalc` is the repair worker for one organization. It
  recalculates repository/object/Actions usage for the current monthly
  period and records an audit snapshot unless the payload explicitly
  skips snapshotting.
- `org:usage_reconcile` is the scheduled/manual fanout job. It lists
  non-deleted organizations in bounded batches and enqueues one
  `org:usage_recalc` job per organization, defaulting the child source
  to `scheduled`.
- Org-owned workflow dispatch recalculates current monthly usage before
  enqueueing. When Actions minutes used is greater than or equal to the
  effective quota, shithub persists a terminal `action_required` workflow
  run/check instead of dispatching runner work, so Code, PR checks, and
  Actions views show the quota gate. Personal repositories are not gated by
  organization quotas.
- Org-owned git pushes measure the actual bare repo directory during
  pre-receive, adjust the recalculated organization counters for that
  repository's current disk size, and reject pushes that would exceed
  the effective storage quota. Personal repositories are not gated by
  organization quotas.
- Org-owned Actions artifact upload URL requests recalculate current
  storage usage before issuing a presigned PUT URL and reject uploads
  whose declared byte count would exceed the effective storage quota.
- Org-owned generic package uploads recalculate current storage usage
  before object storage writes and reject uploads whose declared byte
  count would exceed the effective storage quota. Personal repositories
  do not expose package publishing in the current web UI.
- Site admins can save or clear temporary storage and Actions minutes
  quota overrides from the billing settings debug panel even when they
  are not organization owners. Overrides are attributed to the actor and
  take effect in entitlement limit calculations immediately. Stripe
  checkout and portal actions remain owner-only.
- Quota enforcement must read local counters and may force a source
  recalculation before rejecting large writes; counters are repairable
  and should not be treated as an eventually-consistent sole authority
  for hard-deny decisions.

PAYMENTS SP10 makes paid launch operationally supportable:

- Team billing requires a Stripe recurring monthly Flat rate Price that
  accepts line-item quantity. shithub sends licensed-seat quantity at
  Checkout and later through explicit license-management flows; ordinary
  membership changes only update local used-seat accounting.
- Pro billing uses a separate recurring single-seat Price. If
  `billing.stripe.pro_price_id` is empty, personal-account checkout is
  unavailable even when Team billing is enabled.
- Public billing policy docs describe plan seats, cancellation,
  downgrade preservation, refund handling, payment-data privacy, and
  support expectations. Operators must adapt and publish final legal
  terms before taking live payments.
- Stripe Dashboard setup, live-mode launch checks, webhook replay,
  subscription drift repair, manual downgrade/upgrade, refund handling,
  past-due handling, quota overage response, and billing outage response
  live in [`runbooks/stripe-billing.md`](./runbooks/stripe-billing.md).
- Billing observability includes Checkout/Portal attempt counters,
  webhook result counters, webhook backlog gauges, past-due principal
  gauges, Team seat-drift gauges, and quota-overage gauges. Alert rules
  live under `shithubd-billing` in
  `deploy/monitoring/prometheus/rules.yml`.

PAYMENTS SP15 adds the GitHub-shaped Billing and licensing management
surface for Team seats:

- Organization settings labels the billing area "Billing and
  licensing". `/organizations/{org}/settings/billing` remains the
  owner-only current-plan overview and links to Stripe Billing Portal
  only for payment method, invoice, and cancellation tasks.
- `/organizations/{org}/settings/billing/licensing` is the local
  licensing surface. It shows licensed seats, used seats, available
  seats, pending invitations, and the organization members currently
  consuming seats. Owners get a Team banner with an Edit menu for Add
  seats and Remove seats.
- `/organizations/{org}/settings/billing/seats/add` and
  `/organizations/{org}/settings/billing/seats/remove` are explicit
  confirmation flows. They show the current seat count, used seats,
  available seats, new total, estimated monthly delta, a Stripe
  current-period preview, and the next monthly total before submit.
- Seat changes are available only for Team organizations with an
  active or trialing local subscription state and a Stripe subscription
  item ID. The handler updates Stripe subscription-item quantity first
  and writes local licensed seats only after Stripe accepts the change.
- Remove seats is hidden/denied when there are no unassigned seats and
  rejects any reduction below current used seats.
- When a Team owner tries to invite a member and no seats are
  available, the People page shows a blocker linking directly to Add
  seats. Pending invitations are still displayed separately and are not
  counted as used seats until invitation-seat charging is explicitly
  implemented.

PAYMENTS SP16 starts the Stripe-correct seat-change contract:

- The Stripe edge exposes explicit Team seat operations:
  `PreviewTeamSeatChange`, `ApplyTeamSeatChange`, and
  `FetchSubscriptionItemQuantity`. The older generic quantity update is
  retained only for repair/backfill-style use and is not used by
  owner-confirmed seat changes.
- Add/remove seat pages call Stripe invoice preview for the target
  subscription item quantity before enabling submit. The confirmation
  POST repeats the preview and then applies the same
  `create_prorations` subscription item update with an idempotency key.
- shithub deliberately uses Stripe's `create_prorations` behavior: the
  current-period charge or credit is created for the subscription and
  appears on the next invoice, rather than forcing immediate collection
  during the seat-change request.
- Local licensed seats are updated only after Stripe accepts the
  subscription-item quantity change. If Stripe rejects the change, the
  local license count remains unchanged and the form returns an error.
- Background member/seat usage sync updates local used-seat snapshots
  only. It does not buy seats or change Stripe quantity without an
  owner confirmation flow.
- Site admins see a Stripe/local seat quantity drift check in the
  billing debug panel. When Stripe's live subscription-item quantity
  differs from local licensed seats, the debug panel offers a repair
  action that copies Stripe's quantity into local entitlement state,
  refuses repairs below current used seats, and records
  `admin_org_billing_seats_repaired` audit metadata. The repair action
  never changes Stripe billing.
- `customer.subscription.updated` preserves a newer owner-confirmed or
  admin-repaired local licensed-seat count when Stripe delivers an
  older subscription quantity event. The webhook still applies status,
  period, and subscription metadata, but does not roll seats backward.
- Stripe invoice webhooks persist billing reason plus proration-line
  totals. Recent invoices label Team seat add/remove charges or
  credits as seat changes while preserving that detail across later
  invoice status events.

PAYMENTS SP17 starts GitHub-shaped plan selection and setup parity:

- `/organizations/plan` renders a data-driven Free / Team / Enterprise
  card surface with GitHub-style recommended seat ranges: Free for
  1-4 seats, Team for 5-10 seats, and Enterprise for 11+ seats.
- The seat advisor defaults Team signup to 5 licensed seats and updates
  plan recommendations plus Free/Team CTA URLs as the owner changes the
  desired seat count. Free CTAs keep `seat_count=1`; Team CTAs carry
  the selected licensed-seat count into setup.
- Team setup accepts `seat_count` on `GET /organizations/new`, defaults
  blank Team setup to 5 seats, previews `$4 x N seats`, and preserves
  the explicit Stripe Checkout handoff shipped by SP04/SP14/SP16. The
  GitHub `plan=business` alias maps to shithub Team.
- The setup form follows GitHub's organization onboarding shape:
  organization account name, owner classification, terms acceptance,
  optional profile/import details, and a `Next: Payment details` submit
  label before hosted Stripe checkout.
- The comparison table carries owner sprint, owner path, and current
  implementation state for every row. Unshipped feature rows are
  labeled `Planned` or `Partially shipped` instead of `Included` so the
  public surface remains tied to the sprint reality.
- Enterprise remains a contact-sales path. Enterprise-only identity,
  compliance, and account-management rows stay deferred until the
  Enterprise contract is defined.

PAYMENTS SP18 ships the first private-repository governance bundle
that Team can truthfully advertise:

- Private org branch-rule writes use entitlement checks for
  `advanced_branch_protection`; public repositories remain generous.
  The shipped branch-rule set includes force-push/deletion controls,
  required pull-request pushes, required status checks, and the
  read-only rulesets API projection.
- Private org required-reviewer writes use `required_reviewers`.
  The same feature covers one required approval and the multi-reviewer
  `>1` path; the handler picks distinct upgrade copy from the attempted
  review count.
- The repo branch settings page renders the current governance state:
  public available, private org Team required, active Team, billing
  action needed, or contact-sales enterprise preview. Downgraded orgs
  keep existing rule data and can remove or clear gated settings.
- The organization plan comparison now marks Branch and tag rules plus
  Multiple reviewers as included for Team. Team-based bypass remains a
  later sprint target.

PAYMENTS SP19 ships code-owner and team-reviewer governance:

- `internal/repos/codeowners` parses CODEOWNERS from the GitHub search
  locations on the PR base commit: `.github/CODEOWNERS`, root
  `CODEOWNERS`, then `docs/CODEOWNERS`.
- Public repositories can request and require code-owner review on
  Free. Private organization repositories require Team; private
  personal repositories require Pro.
- CODEOWNERS user owners must resolve to users with effective write
  access. CODEOWNERS team owners must belong to the repository owner
  organization and have explicit `write`, `maintain`, or `admin`
  team-repo access.
- Pull request review requests now support teams. Team requests are
  satisfied when a current team member submits an approval or request
  changes review.
- Branch settings expose "Require review from Code Owners" as a
  branch-only rule control. Downgrades preserve existing configuration
  and block only writes that expand gated private-repo CODEOWNERS
  requirements.

PAYMENTS SP20 ships scheduled PR review reminders:

- Organization owners manage reminders from
  `/organizations/{org}/settings/scheduled-reminders`.
- A reminder can target all organization repositories, a single
  organization repository, or one organization team, and can notify for
  direct user review requests, team review requests, or both after a
  configured minimum age.
- Public repository reminder schedules are available on Free.
  Schedules whose target includes private organization repositories
  require the Team `scheduled_reminders` entitlement.
- Delivery is worker-backed through `org:scheduled_reminder_sweep`.
  The worker claims due schedules, creates idempotent delivery rows,
  re-checks recipient repository access, skips suspended users, and
  writes `scheduled_reminder` inbox notifications on the PR thread.

PAYMENTS SP26 ships the first organization Secret Protection baseline:

- Public repositories keep supported-pattern secret scanning and push
  protection on Free.
- Private organization repositories require Team for historical scan
  views, on-demand scans, organization security overview aggregation,
  and pre-receive push protection.
- The scanner uses the local curated pattern set documented in
  [`secret-protection.md`](./secret-protection.md). Custom patterns,
  bypass-request workflows, and scan-history APIs are shipped. Provider
  notification and provider-side validity checks are explicitly not available
  in the current paid-organization launch scope. The UI and API expose
  truthful unsupported capability states only; no outbound provider integration
  is shipped and Team is not advertised as including those surfaces.

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

Expected org feature keys:

- `secret_teams`
- `advanced_branch_protection`
- `required_reviewers`
- `codeowners_review`
- `actions_org_secrets`
- `actions_org_variables`
- `private_collaboration_limit`
- `storage_quota`
- `actions_minutes_quota`
- `scheduled_reminders`
- `repo_projects`
- `repo_wikis`
- `multiple_assignees`
- `secret_scanning`
- `secret_push_protection`
- `secret_custom_patterns`
- `secret_bypass_controls`

SP21 collaboration gates follow the same public/private split as other
Team rows. Public repository Projects, Wikis, and multiple assignees are
available on Free. Private organization repository writes for projects,
wiki pages, and expanding an issue or PR beyond one assignee require
Team. Downgrade preserves existing data and blocks expansion rather
than deleting projects, wiki pages, or assignee rows.

The deprecated `FeatureOrg*` aliases still compile for older call sites,
but new code should use the canonical unprefixed keys above and let
`FeatureAppliesToKind` decide whether a feature is valid for orgs,
users, or both.

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

- Whether the v1 storage quota should eventually split repository bytes
  from Actions/artifact bytes in the UI. The first SP08 implementation
  tracks both separately but enforces a combined storage quota.

## Source references

- GitHub pricing: `https://github.com/pricing`
- GitHub plans docs:
  `https://docs.github.com/en/get-started/learning-about-github/githubs-plans`
- Stripe Billing: `https://docs.stripe.com/billing`
- Stripe pricing models:
  `https://docs.stripe.com/products-prices/pricing-models`
- Stripe subscription quantities:
  `https://docs.stripe.com/billing/subscriptions/quantities`

## PRO08 GA audit closure

**Audit dates**: 2026-05-13 (run + remediation in a single sprint).

**Scope**: PRO04 Stripe adapter + PRO05 entitlements Principal +
PRO06 user-tier billing settings + PRO07 Pro v1 feature gates.

**Methodology**: four parallel security/correctness audit agents
ran read-only inspections against the PRO07 tip plus the deployed
host (`shithub.sh`). Findings were triaged per-bug; HIGH and
MEDIUM severity gaps were fixed inline on the `payments-pro/08-pro-ga-audit`
branch with locking tests. LOW-severity items were either fixed or
documented per the user directive "we don't want to take any chances
with payments."

### Findings + remediation status

| # | Severity | Finding | Fix | Test |
|---|---|---|---|---|
| A1 | MED | `guardPriceKindMatch` bypassable on empty `Items` | refuse empty-items when prices configured | `TestBillingWebhookGuardRefusesEmptyItemsWhenPricesConfigured` |
| A2 | HIGH | `(subject_kind, subject_id)` never written to receipts | `SetWebhookEventSubjectForPrincipal` after every resolve; `ListFailedWebhookEvents` operator query | `TestSetWebhookEventSubjectForPrincipalRecordsAuditTrail`, `TestListFailedWebhookEventsReturnsErroredAndStuckEntries` |
| A3 | MED | Concurrent dup-replay TOCTOU race | session-scoped advisory lock keyed on event id | `TestBillingWebhookConcurrentReplayAppliesOnce` |
| D1 | HIGH | Snapshot CTE wipes lock columns on past_due transitions | conditional preservation in `ApplySubscriptionSnapshot` + user analog | `TestApplySubscriptionSnapshotPreservesGraceLockOnPastDue` (+ recovery + user-side mirror) |
| D2 | LOW | No `charge.refunded` handler | new dispatch + `MarkInvoiceRefunded` query + enum + UI surface | `TestBillingWebhookChargeRefundedMarksInvoiceRefunded` (+ unknown invoice + standalone refund) |
| D3 | MED | Two subscriptions per customer silently overwrite | `guardSubscriptionOverwrite` rejects different-sub apply | `TestBillingWebhookGuardRefusesSecondSubscriptionForSameCustomer` |
| D4 | MED | No stale-event detection (reverse-order corruption) | `last_event_at` column + `IsBillingEventStaleForPrincipal` + handler guard + post-apply touch | `TestBillingWebhookDropsStaleEvent` |
| D5 | LOW | `customer.subscription.deleted` for unknown sub 5xx's | log + 200 no-op so Stripe stops retrying | `TestBillingWebhookSubscriptionDeletedForUnknownSubIsNoOp` |
| C1 | HIGH | `require_signed_commits` unreachable from UI | form field + template checkbox + sqlc plumbing | covered by `TestSettingsBranches_UserKindEnforceFlipPreventForcePushBlocks` table |
| C2 | HIGH | Advanced BP gate fires on wrong inputs | rewire predicate to fire on prevent_*, signing, AND status-checks | table test |
| C3 | MED | Multi-reviewer deny copy indistinguishable | thread reviewer count → `required-reviewers-multi-upgrade(-pro)` codes | `TestSettingsBranches_UserKindEnforceFlipMultiReviewerBlocks` |
| C4 | MED | Notice copy says "Team" / "organization" on user-tier denies | user-kind variants (`-pro` suffix) with `/settings/billing` href | `TestSettingsBranches_UserKindEnforceFlipRequiredReviewersBlocksSingle` (Pro copy) |
| C5 | LOW | `profilePinsRemaining` hard-codes cap; under-counts for Pro | thread entitled cap into the function | covered by existing profile_test suite + no regression |

Authorization audit (Agent B) found **no** cross-tenant data leak,
no invoice-scoping bleed, and no deny-shape 500 reachable from
production paths. Two `err→500` branches in the entitlement gate
remain (in `RequirePrincipalFeature` and `evaluateBranchProtectionFeature`)
but are defended against by the AFTER-INSERT seed triggers on
both billing-state tables — `pgx.ErrNoRows` for a valid principal
is dead code in production.

### Pre-existing failures NOT introduced by PRO08

These were noted across prior sprints and remain open:

- `TestPrivateCollaborationExpansionEnforcesFreeLimitAndTeamUnlimited`
- `TestPrivateCollaborationUsageCountsEffectivePrivateAccess`
- `TestRepoPrivateVisibilityCountsRepoSpecificGrants`

The fourth pre-existing failure
(`TestSettingsBranchesAllowsDowngradedOrgToRemoveAdvancedSettings`)
was fixed opportunistically while in the branch-protection cluster
(seed fixture needed `AllowedPusherUserIds: []int64{}` to satisfy
the NOT NULL constraint).

### Go/no-go: GA

**GO**, conditional on the operator completing the Stripe Dashboard
checklist documented in `docs/internal/runbooks/stripe-billing.md`
under "Subject resolution chain" and "Per-feature enforcement flags"
sections. Production binary needs redeploy to pick up:

- Migrations 0077 (`last_event_at`) + 0078 (`refunded` enum + column)
- The 13 code fixes above

Deploy steps:

1. `ssh root@shithub.sh "sudo -u shithub /usr/local/bin/shithubd migrate up"` — applies 0077 + 0078
2. Restart `shithubd-web.service` and `shithubd-worker.service`
3. Verify with: `SELECT version_id FROM goose_db_version ORDER BY version_id DESC LIMIT 1` → expect `78`
4. Configure Stripe env vars per the runbook (still in test mode until ratified)
5. Run the smoke checklist in `runbooks/stripe-billing.md#smoke-test`
6. Flip enforce flags per feature after 7-day report-only soak

**No production customers exist today** (`SELECT plan, count(*) FROM users WHERE deleted_at IS NULL` → `free: 2`; same for orgs), so the deploy carries near-zero risk to existing customers — there are none on Pro/Team plans.
