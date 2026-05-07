-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Account-settings columns:
--
-- - theme: persisted theme preference (cookie is the fast path, DB is
--   the source of truth across devices). NULL → fall back to cookie/auto.
-- - session_epoch: bumped by "log out everywhere" so existing cookies
--   carrying a stale epoch fail validation on the next request.
--
-- The username-change rate-limit window is implicit in
-- username_redirects.changed_at — we count rows in the last 60 days
-- per user to enforce the 3/60d cap. No new column needed.

-- +goose Up
ALTER TABLE users
    ADD COLUMN theme         text NOT NULL DEFAULT '',
    ADD COLUMN session_epoch integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT users_theme_known CHECK (theme IN ('', 'light', 'dark', 'auto', 'high_contrast'));

-- +goose Down
ALTER TABLE users
    DROP COLUMN IF EXISTS theme,
    DROP COLUMN IF EXISTS session_epoch;
