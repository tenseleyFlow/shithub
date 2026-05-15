-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-05: vanity username reservations. Pro users can reserve up
-- to 3 inactive usernames (handles they do not currently own) against
-- squatting. The reservation BLOCKS signup + rename for the reserved
-- handle anywhere else on the system.
--
-- Existing squat-protection surfaces:
--   - users.username (citext, unique) — active users.
--   - username_redirects.old_username — renamed-from handles, the
--     redirect doubles as a permanent reservation.
--   - internal/auth/reserved.go — hardcoded system names.
--
-- This table is the fourth: handles a Pro user proactively reserved
-- WITHOUT ever holding. Signup + rename code paths consult this in
-- addition to the three above.
--
-- The reserved_handle column is citext so the uniqueness check matches
-- users.username's case-insensitive semantics. The unique constraint
-- across the whole table guarantees one user owns the reservation;
-- multiple Pro users cannot reserve the same handle.

-- +goose Up
CREATE TABLE user_username_reservations (
    id               bigserial   PRIMARY KEY,
    user_id          bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reserved_handle  citext      NOT NULL UNIQUE,
    created_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_username_reservations_handle_shape CHECK (
        -- Mirrors usernameRE in the auth handlers: 1-39 chars,
        -- lowercase letters / digits / hyphens, no leading or trailing
        -- hyphen. Defense-in-depth against a handler bypass.
        reserved_handle ~ '^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$'
    )
);

CREATE INDEX user_username_reservations_user_id_idx
    ON user_username_reservations (user_id);

-- +goose Down
DROP TABLE IF EXISTS user_username_reservations;
