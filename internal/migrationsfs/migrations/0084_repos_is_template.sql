-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-06pre: template repository base feature. Adds the
-- is_template flag to the repos table. The handler is responsible for
-- the entitlement gate (PRO-EXT01-06 adds it on top); the column
-- itself is unconstrained beyond NOT NULL.

-- +goose Up
ALTER TABLE repos
    ADD COLUMN is_template boolean NOT NULL DEFAULT false;

-- Partial index so the (rare) lookup "list templates an owner has
-- published" is cheap. Templates are a small fraction of all repos;
-- a partial index avoids bloating the all-repos working set.
CREATE INDEX repos_is_template_partial_idx
    ON repos (id)
    WHERE is_template = true;

-- +goose Down
DROP INDEX IF EXISTS repos_is_template_partial_idx;
ALTER TABLE repos DROP COLUMN is_template;
