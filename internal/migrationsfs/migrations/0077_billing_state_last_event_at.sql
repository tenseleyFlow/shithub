-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO08 — record Stripe's event.created timestamp on each
-- billing-state row so the webhook handler can refuse stale events.
--
-- Stripe does not guarantee delivery order for retries — a reverse-
-- ordered pair (subscription.updated[canceled] then
-- subscription.updated[active]) would re-activate a canceled
-- subscription if the handler applies them in arrival order. The
-- guard compares event.Created against the persisted last_event_at
-- and refuses the apply when the incoming event is older.
--
-- Nullable by design:
-- - Legacy rows (pre-PRO08) have no observed events — NULL means
--   "no prior event; accept anything."
-- - Rows for subjects that haven't received any Stripe event yet
--   also stay NULL until the first apply.

-- +goose Up
ALTER TABLE org_billing_states
    ADD COLUMN last_event_at timestamptz;
ALTER TABLE user_billing_states
    ADD COLUMN last_event_at timestamptz;

-- +goose Down
ALTER TABLE user_billing_states DROP COLUMN IF EXISTS last_event_at;
ALTER TABLE org_billing_states DROP COLUMN IF EXISTS last_event_at;
