-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO03 — user billing state table.
--
-- Mirrors org_billing_states minus the seat-specific columns
-- (billable_seats, seat_snapshot_at). Pro is a single-seat plan by
-- design; there's no seat reconciliation worker for users. The
-- billing_subscription_status and billing_lock_reason enums from
-- 0061 are subject-agnostic and reused as-is.
--
-- Stripe customer-id namespace is global per Stripe account; the
-- partial-unique indexes on each table (org_billing_states +
-- user_billing_states) prevent a single customer-id from existing
-- on both tables. Defensive cross-table validation lands in PRO04's
-- webhook handler.

-- +goose Up

CREATE TABLE user_billing_states (
    user_id                     bigint                       PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    provider                    billing_provider             NOT NULL DEFAULT 'stripe',
    stripe_customer_id          text,
    stripe_subscription_id      text,
    stripe_subscription_item_id text,
    plan                        user_plan                    NOT NULL DEFAULT 'free',
    subscription_status         billing_subscription_status  NOT NULL DEFAULT 'none',
    current_period_start        timestamptz,
    current_period_end          timestamptz,
    cancel_at_period_end        boolean                      NOT NULL DEFAULT false,
    trial_end                   timestamptz,
    past_due_at                 timestamptz,
    canceled_at                 timestamptz,
    locked_at                   timestamptz,
    lock_reason                 billing_lock_reason,
    grace_until                 timestamptz,
    last_webhook_event_id       text                         NOT NULL DEFAULT '',
    created_at                  timestamptz                  NOT NULL DEFAULT now(),
    updated_at                  timestamptz                  NOT NULL DEFAULT now(),

    CONSTRAINT user_billing_states_customer_id_not_blank CHECK (
        stripe_customer_id IS NULL OR char_length(stripe_customer_id) > 0
    ),
    CONSTRAINT user_billing_states_subscription_id_not_blank CHECK (
        stripe_subscription_id IS NULL OR char_length(stripe_subscription_id) > 0
    ),
    CONSTRAINT user_billing_states_subscription_item_id_not_blank CHECK (
        stripe_subscription_item_id IS NULL OR char_length(stripe_subscription_item_id) > 0
    ),
    CONSTRAINT user_billing_states_lock_reason_requires_locked CHECK (
        lock_reason IS NULL OR locked_at IS NOT NULL
    ),
    CONSTRAINT user_billing_states_grace_requires_locked CHECK (
        grace_until IS NULL OR locked_at IS NOT NULL
    ),
    CONSTRAINT user_billing_states_period_order CHECK (
        current_period_start IS NULL
     OR current_period_end IS NULL
     OR current_period_start <= current_period_end
    )
);

CREATE UNIQUE INDEX user_billing_states_stripe_customer_unique
    ON user_billing_states (stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

CREATE UNIQUE INDEX user_billing_states_stripe_subscription_unique
    ON user_billing_states (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

CREATE UNIQUE INDEX user_billing_states_stripe_subscription_item_unique
    ON user_billing_states (stripe_subscription_item_id)
    WHERE stripe_subscription_item_id IS NOT NULL;

CREATE INDEX user_billing_states_status_idx
    ON user_billing_states (subscription_status, updated_at DESC);

CREATE INDEX user_billing_states_locked_idx
    ON user_billing_states (locked_at)
    WHERE locked_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON user_billing_states
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down

DROP TRIGGER IF EXISTS set_updated_at ON user_billing_states;
DROP INDEX IF EXISTS user_billing_states_locked_idx;
DROP INDEX IF EXISTS user_billing_states_status_idx;
DROP INDEX IF EXISTS user_billing_states_stripe_subscription_item_unique;
DROP INDEX IF EXISTS user_billing_states_stripe_subscription_unique;
DROP INDEX IF EXISTS user_billing_states_stripe_customer_unique;
DROP TABLE IF EXISTS user_billing_states;
