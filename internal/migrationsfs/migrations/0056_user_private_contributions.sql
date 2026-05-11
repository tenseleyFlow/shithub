-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Profile contribution privacy:
--
-- - include_private_contributions mirrors GitHub's profile setting for
--   displaying private contribution counts on the public contribution graph.
--   The graph never exposes private repository names or commit metadata.

-- +goose Up
ALTER TABLE users
    ADD COLUMN include_private_contributions boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE users
    DROP COLUMN IF EXISTS include_private_contributions;
