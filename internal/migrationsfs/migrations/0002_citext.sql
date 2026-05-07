-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Enable the citext extension. Used by users.username and user_emails.email
-- so case-insensitive uniqueness is enforced at the type level (no
-- lower(col) functional indexes, no application-side normalization).

-- +goose Up
CREATE EXTENSION IF NOT EXISTS citext;

-- +goose Down
-- Intentionally left empty: dropping citext would cascade across columns
-- created in later migrations. If a full down-migration is required, drop
-- the dependent tables first.
