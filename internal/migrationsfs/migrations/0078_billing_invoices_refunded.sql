-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO08 D2 — surface refunds in shithub.
--
-- Stripe handles refunds out-of-band: a `charge.refunded` event fires,
-- but the underlying invoice's `status` stays `paid`. shithub wants
-- a UI surface ("Refunded $X on date Y") on the billing settings
-- page, so we track refunds locally:
--
-- - `refunded_at` on billing_invoices records when the operator (or
--   the cardholder) issued the refund.
-- - The status enum gains a `refunded` value the UI can switch on.
--
-- Refund handling does NOT automatically cancel the subscription.
-- An operator issuing a refund may want to keep Pro active (e.g.,
-- a goodwill refund). Subscription cancellation is a separate Stripe
-- action — Stripe's customer.subscription.deleted handler handles
-- that path. This migration is UI surface only.

-- +goose Up
-- Postgres requires ALTER TYPE ... ADD VALUE outside a transaction.
-- The migration is no-tx by goose default for this statement.

-- +goose NO TRANSACTION
ALTER TYPE billing_invoice_status ADD VALUE IF NOT EXISTS 'refunded';

-- +goose StatementBegin
ALTER TABLE billing_invoices
    ADD COLUMN IF NOT EXISTS refunded_at timestamptz;
-- +goose StatementEnd

-- +goose Down
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS refunded_at;
-- Note: Postgres does not support DROP VALUE on an enum, so the
-- 'refunded' enum entry is permanent. Down migrations that depend
-- on it require a full type recreate; for now we leave the value.
