-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS SP02 — local billing domain.
--
-- Stripe remains the payment source of truth, but shithub needs local
-- state for entitlement checks, UI summaries, idempotent webhook
-- processing, and seat snapshots. These tables deliberately store
-- provider IDs and invoice metadata only; card data never touches
-- shithub.

-- +goose Up

CREATE TYPE billing_provider AS ENUM ('stripe');

CREATE TYPE billing_subscription_status AS ENUM (
    'none',
    'incomplete',
    'trialing',
    'active',
    'past_due',
    'canceled',
    'unpaid',
    'paused'
);

CREATE TYPE billing_lock_reason AS ENUM (
    'past_due',
    'canceled',
    'unpaid',
    'manual'
);

CREATE TYPE billing_invoice_status AS ENUM (
    'draft',
    'open',
    'paid',
    'void',
    'uncollectible'
);

CREATE TABLE org_billing_states (
    org_id                      bigint                       PRIMARY KEY REFERENCES orgs(id) ON DELETE CASCADE,
    provider                    billing_provider             NOT NULL DEFAULT 'stripe',
    stripe_customer_id          text,
    stripe_subscription_id      text,
    stripe_subscription_item_id text,
    plan                        org_plan                     NOT NULL DEFAULT 'free',
    subscription_status         billing_subscription_status   NOT NULL DEFAULT 'none',
    billable_seats              integer                      NOT NULL DEFAULT 0,
    seat_snapshot_at            timestamptz,
    current_period_start        timestamptz,
    current_period_end          timestamptz,
    cancel_at_period_end        boolean                      NOT NULL DEFAULT false,
    trial_end                   timestamptz,
    past_due_at                 timestamptz,
    canceled_at                 timestamptz,
    locked_at                   timestamptz,
    lock_reason                 billing_lock_reason,
    grace_until                 timestamptz,
    last_webhook_event_id       text                          NOT NULL DEFAULT '',
    created_at                  timestamptz                   NOT NULL DEFAULT now(),
    updated_at                  timestamptz                   NOT NULL DEFAULT now(),

    CONSTRAINT org_billing_states_seats_nonnegative CHECK (billable_seats >= 0),
    CONSTRAINT org_billing_states_customer_id_not_blank CHECK (
        stripe_customer_id IS NULL OR char_length(stripe_customer_id) > 0
    ),
    CONSTRAINT org_billing_states_subscription_id_not_blank CHECK (
        stripe_subscription_id IS NULL OR char_length(stripe_subscription_id) > 0
    ),
    CONSTRAINT org_billing_states_subscription_item_id_not_blank CHECK (
        stripe_subscription_item_id IS NULL OR char_length(stripe_subscription_item_id) > 0
    ),
    CONSTRAINT org_billing_states_lock_reason_requires_locked CHECK (
        lock_reason IS NULL OR locked_at IS NOT NULL
    ),
    CONSTRAINT org_billing_states_grace_requires_locked CHECK (
        grace_until IS NULL OR locked_at IS NOT NULL
    ),
    CONSTRAINT org_billing_states_period_order CHECK (
        current_period_start IS NULL
     OR current_period_end IS NULL
     OR current_period_start <= current_period_end
    )
);

CREATE UNIQUE INDEX org_billing_states_stripe_customer_unique
    ON org_billing_states (stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

CREATE UNIQUE INDEX org_billing_states_stripe_subscription_unique
    ON org_billing_states (stripe_subscription_id)
    WHERE stripe_subscription_id IS NOT NULL;

CREATE UNIQUE INDEX org_billing_states_stripe_subscription_item_unique
    ON org_billing_states (stripe_subscription_item_id)
    WHERE stripe_subscription_item_id IS NOT NULL;

CREATE INDEX org_billing_states_status_idx
    ON org_billing_states (subscription_status, updated_at DESC);

CREATE INDEX org_billing_states_locked_idx
    ON org_billing_states (locked_at)
    WHERE locked_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON org_billing_states
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- Backfill current organizations as Free. orgs.plan remains the
-- human-facing summary; billing state starts conservative until Stripe
-- webhooks activate a paid subscription.
INSERT INTO org_billing_states (org_id, plan)
SELECT id, 'free'::org_plan
FROM orgs
ON CONFLICT (org_id) DO NOTHING;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tg_org_billing_state_seed() RETURNS trigger AS $$
BEGIN
    INSERT INTO org_billing_states (org_id, plan)
    VALUES (NEW.id, 'free'::org_plan)
    ON CONFLICT (org_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER tg_org_billing_state_seed_ai
    AFTER INSERT ON orgs
    FOR EACH ROW EXECUTE FUNCTION tg_org_billing_state_seed();

CREATE TABLE billing_seat_snapshots (
    id                         bigserial          PRIMARY KEY,
    org_id                     bigint             NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider                   billing_provider   NOT NULL DEFAULT 'stripe',
    stripe_subscription_id     text,
    active_members             integer            NOT NULL,
    billable_seats             integer            NOT NULL,
    source                     text               NOT NULL DEFAULT 'local',
    captured_at                timestamptz        NOT NULL DEFAULT now(),

    CONSTRAINT billing_seat_snapshots_active_members_nonnegative CHECK (active_members >= 0),
    CONSTRAINT billing_seat_snapshots_billable_seats_nonnegative CHECK (billable_seats >= 0),
    CONSTRAINT billing_seat_snapshots_source_length CHECK (char_length(source) BETWEEN 1 AND 64)
);

CREATE INDEX billing_seat_snapshots_org_captured_idx
    ON billing_seat_snapshots (org_id, captured_at DESC);

CREATE TABLE billing_invoices (
    id                         bigserial               PRIMARY KEY,
    org_id                     bigint                  NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    provider                   billing_provider        NOT NULL DEFAULT 'stripe',
    stripe_invoice_id          text                    NOT NULL,
    stripe_customer_id         text                    NOT NULL,
    stripe_subscription_id     text,
    status                     billing_invoice_status  NOT NULL,
    number                     text                    NOT NULL DEFAULT '',
    currency                   text                    NOT NULL,
    amount_due_cents           bigint                  NOT NULL DEFAULT 0,
    amount_paid_cents          bigint                  NOT NULL DEFAULT 0,
    amount_remaining_cents     bigint                  NOT NULL DEFAULT 0,
    hosted_invoice_url         text                    NOT NULL DEFAULT '',
    invoice_pdf_url            text                    NOT NULL DEFAULT '',
    period_start               timestamptz,
    period_end                 timestamptz,
    due_at                     timestamptz,
    paid_at                    timestamptz,
    voided_at                  timestamptz,
    created_at                 timestamptz             NOT NULL DEFAULT now(),
    updated_at                 timestamptz             NOT NULL DEFAULT now(),

    CONSTRAINT billing_invoices_stripe_invoice_not_blank CHECK (char_length(stripe_invoice_id) > 0),
    CONSTRAINT billing_invoices_stripe_customer_not_blank CHECK (char_length(stripe_customer_id) > 0),
    CONSTRAINT billing_invoices_currency_iso CHECK (
        char_length(currency) = 3 AND currency = lower(currency)
    ),
    CONSTRAINT billing_invoices_amounts_nonnegative CHECK (
        amount_due_cents >= 0
        AND amount_paid_cents >= 0
        AND amount_remaining_cents >= 0
    ),
    CONSTRAINT billing_invoices_period_order CHECK (
        period_start IS NULL OR period_end IS NULL OR period_start <= period_end
    ),

    UNIQUE (provider, stripe_invoice_id)
);

CREATE INDEX billing_invoices_org_created_idx
    ON billing_invoices (org_id, created_at DESC);

CREATE INDEX billing_invoices_status_idx
    ON billing_invoices (status, created_at DESC);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON billing_invoices
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

CREATE TABLE billing_webhook_events (
    id                         bigserial          PRIMARY KEY,
    provider                   billing_provider   NOT NULL DEFAULT 'stripe',
    provider_event_id          text               NOT NULL,
    event_type                 text               NOT NULL,
    api_version                text               NOT NULL DEFAULT '',
    payload                    jsonb              NOT NULL DEFAULT '{}'::jsonb,
    received_at                timestamptz        NOT NULL DEFAULT now(),
    processed_at               timestamptz,
    process_error              text               NOT NULL DEFAULT '',
    processing_attempts        integer            NOT NULL DEFAULT 0,

    CONSTRAINT billing_webhook_events_provider_event_not_blank CHECK (char_length(provider_event_id) > 0),
    CONSTRAINT billing_webhook_events_type_not_blank CHECK (char_length(event_type) > 0),
    CONSTRAINT billing_webhook_events_attempts_nonnegative CHECK (processing_attempts >= 0),
    CONSTRAINT billing_webhook_events_payload_object CHECK (jsonb_typeof(payload) = 'object'),

    UNIQUE (provider, provider_event_id)
);

CREATE INDEX billing_webhook_events_received_idx
    ON billing_webhook_events (received_at DESC);

CREATE INDEX billing_webhook_events_processed_idx
    ON billing_webhook_events (processed_at)
    WHERE processed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS billing_webhook_events_processed_idx;
DROP INDEX IF EXISTS billing_webhook_events_received_idx;
DROP TABLE IF EXISTS billing_webhook_events;

DROP TRIGGER IF EXISTS set_updated_at ON billing_invoices;
DROP INDEX IF EXISTS billing_invoices_status_idx;
DROP INDEX IF EXISTS billing_invoices_org_created_idx;
DROP TABLE IF EXISTS billing_invoices;

DROP INDEX IF EXISTS billing_seat_snapshots_org_captured_idx;
DROP TABLE IF EXISTS billing_seat_snapshots;

DROP TRIGGER IF EXISTS tg_org_billing_state_seed_ai ON orgs;
DROP FUNCTION IF EXISTS tg_org_billing_state_seed();
DROP TRIGGER IF EXISTS set_updated_at ON org_billing_states;
DROP INDEX IF EXISTS org_billing_states_locked_idx;
DROP INDEX IF EXISTS org_billing_states_status_idx;
DROP INDEX IF EXISTS org_billing_states_stripe_subscription_item_unique;
DROP INDEX IF EXISTS org_billing_states_stripe_subscription_unique;
DROP INDEX IF EXISTS org_billing_states_stripe_customer_unique;
DROP TABLE IF EXISTS org_billing_states;

DROP TYPE IF EXISTS billing_invoice_status;
DROP TYPE IF EXISTS billing_lock_reason;
DROP TYPE IF EXISTS billing_subscription_status;
DROP TYPE IF EXISTS billing_provider;
