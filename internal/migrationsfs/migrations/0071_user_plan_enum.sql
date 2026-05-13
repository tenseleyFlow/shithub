-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PAYMENTS PRO03 — user-tier plan enum.
--
-- Personal accounts get a `user_plan` enum mirroring `org_plan`'s
-- role, but with values disjoint from the org tier. PRO02 Q2 ratified
-- separate enums: Free orgs and Free personal accounts share a label
-- but have different semantics (orgs carry seat counts; users don't),
-- and future SKUs on one side shouldn't pollute the other's namespace.
--
-- `users.plan` defaults to 'free'. Existing rows pick up the default
-- automatically; no explicit backfill needed.

-- +goose Up

CREATE TYPE user_plan AS ENUM ('free', 'pro');

ALTER TABLE users
    ADD COLUMN plan user_plan NOT NULL DEFAULT 'free';

CREATE INDEX users_plan_idx ON users (plan) WHERE plan <> 'free';

-- +goose Down

DROP INDEX IF EXISTS users_plan_idx;
ALTER TABLE users DROP COLUMN IF EXISTS plan;
DROP TYPE IF EXISTS user_plan;
