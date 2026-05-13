-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO03 — make billing_invoices polymorphic over subject.
--
-- PRO02 Q1 ratified the hybrid table strategy: invoices and
-- webhook-events go polymorphic (their UNIQUE indexes were already
-- subject-agnostic), org_billing_states stays per-subject. After this
-- migration billing_invoices carries (subject_kind, subject_id)
-- alongside the legacy org_id column.
--
-- **Two-step deploy.** This migration adds the new columns and
-- backfills them but KEEPS org_id and its FK. A follow-up migration
-- (post-PRO04 deploy) drops org_id once every call site reads from
-- the polymorphic shape. Dropping it here would force a flag-day
-- deploy where every reader/writer must be in lockstep with this
-- migration.

-- +goose Up

CREATE TYPE billing_subject_kind AS ENUM ('user', 'org');

ALTER TABLE billing_invoices
    ADD COLUMN subject_kind billing_subject_kind,
    ADD COLUMN subject_id   bigint;

-- Backfill every existing row as an org invoice. Synchronous so the
-- NOT NULL constraints below apply against a fully-populated column.
UPDATE billing_invoices
SET subject_kind = 'org',
    subject_id   = org_id
WHERE subject_kind IS NULL;

ALTER TABLE billing_invoices
    ALTER COLUMN subject_kind SET NOT NULL,
    ALTER COLUMN subject_id   SET NOT NULL;

-- Cross-row consistency: the legacy org_id is preserved during the
-- transitional window; while it exists, it must match subject_id when
-- subject_kind='org'. Future invoice rows (PRO04+) for users carry
-- org_id=NULL — relax the FK by making it nullable BEFORE this check
-- so existing org rows stay valid and user rows can land.
ALTER TABLE billing_invoices
    ALTER COLUMN org_id DROP NOT NULL;

ALTER TABLE billing_invoices
    ADD CONSTRAINT billing_invoices_org_id_matches_subject CHECK (
        org_id IS NULL
        OR (subject_kind = 'org' AND subject_id = org_id)
    );

-- New index for the polymorphic invoice-listing queries; mirrors the
-- shape of the existing billing_invoices_org_created_idx.
CREATE INDEX billing_invoices_subject_created_idx
    ON billing_invoices (subject_kind, subject_id, created_at DESC);

-- +goose Down

DROP INDEX IF EXISTS billing_invoices_subject_created_idx;
ALTER TABLE billing_invoices DROP CONSTRAINT IF EXISTS billing_invoices_org_id_matches_subject;
ALTER TABLE billing_invoices ALTER COLUMN org_id SET NOT NULL;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS subject_id;
ALTER TABLE billing_invoices DROP COLUMN IF EXISTS subject_kind;
DROP TYPE IF EXISTS billing_subject_kind;
