-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO03 — add subject audit columns to billing_webhook_events.
--
-- The webhook idempotency table is already subject-agnostic at the
-- data-model level: it dedupes by (provider, provider_event_id) and
-- has no org FK. This migration only adds (subject_kind, subject_id)
-- audit columns so future operator queries can answer "which user/org
-- did this event apply to?" without re-parsing the payload.
--
-- Nullable by design:
-- - Legacy rows (pre-PRO04) have no resolved subject — they predate
--   the metadata convention.
-- - Webhook events that fail to resolve a subject (corrupted
--   metadata, customer-id not found on either side) still get a
--   receipt row so they aren't replayed forever. Such rows carry
--   NULL subject and a non-empty process_error.

-- +goose Up

ALTER TABLE billing_webhook_events
    ADD COLUMN subject_kind billing_subject_kind,
    ADD COLUMN subject_id   bigint;

-- Both-or-neither: a receipt row either has a resolved subject (both
-- columns populated) or doesn't (both NULL). Mixed state means a bug
-- in the resolver.
ALTER TABLE billing_webhook_events
    ADD CONSTRAINT billing_webhook_events_subject_both_or_neither CHECK (
        (subject_kind IS NULL AND subject_id IS NULL)
        OR (subject_kind IS NOT NULL AND subject_id IS NOT NULL)
    );

-- Operator-query index for "events that did resolve a subject."
CREATE INDEX billing_webhook_events_subject_idx
    ON billing_webhook_events (subject_kind, subject_id, received_at DESC)
    WHERE subject_kind IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS billing_webhook_events_subject_idx;
ALTER TABLE billing_webhook_events DROP CONSTRAINT IF EXISTS billing_webhook_events_subject_both_or_neither;
ALTER TABLE billing_webhook_events DROP COLUMN IF EXISTS subject_id;
ALTER TABLE billing_webhook_events DROP COLUMN IF EXISTS subject_kind;
