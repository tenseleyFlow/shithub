-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- E-audit E7: surface and persist the repo homepage URL. The CLI's
-- `repo edit --homepage` flag has always been a no-op on the server
-- because the column didn't exist; PATCH silently dropped the field
-- and `repo view --json homepage` returned empty. Add the column,
-- size it for the longest sensible URL, and default to empty string
-- so existing rows backfill cleanly.

-- +goose Up
ALTER TABLE repos
    ADD COLUMN homepage text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE repos
    DROP COLUMN homepage;
