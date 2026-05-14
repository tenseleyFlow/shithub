# Pro for personal accounts

Pro is shithub's single-seat paid plan for personal accounts. It
unlocks a small set of features beyond what Free offers, charged at
$4 / month and managed entirely through the Stripe Billing Portal.

Upgrade, downgrade, and invoice management live at
[`/settings/billing`](../user/account.md).

Organization Team billing is separate from Pro and is billed per active
organization member. See the [billing policies](./billing-policies.md)
for the cancellation, refund, payment processing, and support contract.

## What Pro unlocks

| Feature | Free | Pro |
|---|---|---|
| Public and private repositories | Included | Included |
| Required reviewers on private personal repos | Upgrade | Included |
| Multi-reviewer (>1 approvals) on private personal repos | Upgrade | Included |
| Advanced branch protection on private personal repos | Upgrade | Included |
| Pinned repositories on your profile | Up to 6 | Up to 100 |

"Advanced branch protection" covers preventing force-pushes,
preventing deletion, and requiring signed commits on private personal
repos. Basic protection (an empty rule, or one with none of those
flags) stays on Free.

## What stays on Free

- Public and private repositories — no count limits.
- Org features (Team plan) — Pro applies to your *personal* account.
  Organizations have a separate Team tier with its own feature set.
- Issues, pull requests, Actions minutes, Storage — none of these are
  gated by Pro today. Pro v1 is intentionally small.

## Downgrading

Cancellation flows through the Stripe Billing Portal. shithub honors
the scheduled cancellation: at the end of the paid period your
account returns to Free.

- Existing required-reviewer rules and advanced branch protection
  flags stay in the database. The gate refuses to **create** new
  gated state on Free, but never deletes prior configuration.
- Profile pins above 6 stay in place — you keep what you pinned as
  Pro; the cap re-applies when you next edit your pins.

## Payment failures

A past-due subscription enters a grace period (operator-configured;
the default is 14 days). Pro features stay available during grace.
After grace lapses, Pro-only features become read-only until billing
is brought back into good standing.

## Operator note

Each Pro feature gate has an independent operator-controlled enforce
flag in `billing.enforce.*`. Until an operator flips a feature on,
its gate runs in report-only mode — Free users continue to use the
feature, the would-deny is logged for the soak. This page describes
the **eventual** Pro behavior; deployment-specific timing depends on
your operator's per-feature rollout schedule.
