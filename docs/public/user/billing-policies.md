# Billing policies

This page describes the hosted shithub billing contract. It is product
policy, not legal advice; hosted operators must publish final terms and
tax language that match their business and jurisdiction before taking
live payments.

## Plans and seats

- Pro is a single-seat plan for a personal account.
- Team is billed per active organization member, including owners.
- Pending invitations are not billed until accepted.
- Removing an organization member should reduce the Stripe subscription
  item quantity after the billing seat-sync worker runs.
- Enterprise is a contact-sales placeholder until the matching product
  and support processes exist.

Team billing is managed by organization owners. Pro billing is managed
by the account holder.

## Payment processing

shithub uses Stripe Checkout and the Stripe Billing Portal for hosted
payments. shithub does not collect or store raw card numbers, CVV
values, or bank details.

The local shithub database stores the Stripe customer ID, subscription
ID, subscription item ID, invoice IDs, invoice status, invoice amounts,
period dates, and signed webhook receipt metadata needed to reconcile
billing state.

Taxes are calculated by Stripe only when the operator enables and
configures Stripe Tax. Prices shown in shithub may not include taxes
unless the operator's public terms say otherwise.

## Cancellations and downgrades

Customers can cancel through the Stripe Billing Portal. shithub honors
scheduled cancellation at the end of the paid period. When a paid plan
ends, the account or organization returns to Free.

Downgrades preserve data. Existing gated settings may become read-only,
but shithub should not delete repositories, teams, branch rules,
secrets, variables, profile pins, or review settings solely because a
subscription ended.

## Refunds

Refunds are handled by the operator in Stripe. A reasonable hosted
default is to review refund requests sent through the billing support
contact within 14 days of an initial charge or renewal.

Refunding an invoice does not automatically cancel a subscription.
Customers who also want to stop future billing must cancel the
subscription in the Billing Portal or ask support to do so.

## Payment failures

Failed payments enter the operator-configured grace period. During
grace, paid features remain available. After grace expires, paid-only
features become read-only or unavailable until billing returns to good
standing.

## Billing support

Hosted operators must publish a support email before taking live
payments. Billing support should cover payment failures, invoice
questions, refund requests, accidental duplicate subscriptions, and
account recovery for paid customers.

Operational incidents that affect billing are tracked in the internal
Stripe billing runbook and should be resolved before customers need to
open support tickets.
