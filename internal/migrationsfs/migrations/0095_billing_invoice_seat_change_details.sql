-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS SP16 — surface seat-change invoice details.
--
-- Stripe prorates Team seat add/remove operations with
-- create_prorations. Those charges or credits appear on invoices as
-- proration line items, but the original billing_invoices projection
-- only stored status and totals. Persist the minimal metadata needed
-- to label "Seat change" invoices in the billing UI without reparsing
-- webhook payload JSON on page render.

-- +goose Up

ALTER TABLE billing_invoices
    ADD COLUMN billing_reason text NOT NULL DEFAULT '',
    ADD COLUMN has_proration boolean NOT NULL DEFAULT false,
    ADD COLUMN proration_amount_cents bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT billing_invoices_billing_reason_not_null CHECK (billing_reason IS NOT NULL);

-- +goose Down

ALTER TABLE billing_invoices
    DROP CONSTRAINT IF EXISTS billing_invoices_billing_reason_not_null,
    DROP COLUMN IF EXISTS proration_amount_cents,
    DROP COLUMN IF EXISTS has_proration,
    DROP COLUMN IF EXISTS billing_reason;
