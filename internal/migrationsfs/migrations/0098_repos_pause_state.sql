-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-15: repo "paused" state.
--
-- Paused is a Pro-exclusive state distinct from archive. Semantics:
--
--   * Reads behave normally — clones, web views, REST GETs all work.
--   * Writes return 402 (Payment Required) at every gate: git push,
--     issue/PR/comment creates, REST mutations. Surfaced via policy
--     deny code DenyPaused (added in the same PR).
--   * Forking a paused source is blocked (mirrors archive: a paused
--     source can't seed a downstream fork until unpaused).
--   * Soft-delete overrides pause — a deleted-then-paused repo would
--     never re-surface, so the deleted state wins.
--   * Pause cannot coexist with archive. A check constraint enforces
--     this: an owner who wants to "freeze and forget" archives; an
--     owner who wants "freeze but plan to come back" pauses.
--   * Billing impact ("billed at a reduced rate") is deferred until
--     `internal/billing/storage.go` exists. The columns land now so
--     that future sprint can compute multipliers without a follow-on
--     migration.
--
-- The triple of columns mirrors archive (is_archived / archived_at):
-- a boolean for the hot path, a timestamp for ordering + UI, an
-- optional human-readable reason the owner can leave on the repo for
-- visitors.

-- +goose Up
ALTER TABLE repos
    ADD COLUMN is_paused boolean NOT NULL DEFAULT false,
    ADD COLUMN paused_at timestamptz,
    ADD COLUMN pause_reason text;

-- A repo cannot be both archived and paused. The two states overlap
-- in intent (read-only) but differ in lifecycle (archive is "done";
-- pause is "I'll be back"). Enforcing mutual exclusion at the DB
-- means the policy gate never has to decide which deny wins.
ALTER TABLE repos
    ADD CONSTRAINT repos_pause_archive_mutex CHECK (
        NOT (is_archived AND is_paused)
    );

-- Pair the boolean and timestamp consistently — `paused_at` must
-- only be set when `is_paused` is true. Same shape as the implicit
-- contract between is_archived/archived_at; here we make it explicit.
ALTER TABLE repos
    ADD CONSTRAINT repos_paused_at_pair CHECK (
        (is_paused AND paused_at IS NOT NULL)
        OR (NOT is_paused AND paused_at IS NULL)
    );

-- The pause_reason cap matches the soft-delete/suspend reason caps
-- elsewhere (e.g. users.suspended_reason). 280 chars = a tweet's
-- worth, plenty for "winter break — back in March".
ALTER TABLE repos
    ADD CONSTRAINT repos_pause_reason_length CHECK (
        pause_reason IS NULL OR char_length(pause_reason) <= 280
    );

-- Hot-path lookup: workers (eventual billing sweep, cleanup) want to
-- enumerate paused repos. Partial index keeps the index tiny — most
-- repos are not paused, so a full b-tree index would waste pages.
CREATE INDEX repos_paused_idx ON repos (paused_at DESC) WHERE is_paused;

-- +goose Down
DROP INDEX IF EXISTS repos_paused_idx;
ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_pause_reason_length;
ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_paused_at_pair;
ALTER TABLE repos DROP CONSTRAINT IF EXISTS repos_pause_archive_mutex;
ALTER TABLE repos
    DROP COLUMN IF EXISTS pause_reason,
    DROP COLUMN IF EXISTS paused_at,
    DROP COLUMN IF EXISTS is_paused;
