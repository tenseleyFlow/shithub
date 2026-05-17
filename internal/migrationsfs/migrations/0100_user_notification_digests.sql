-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-16b: scheduled digest emails for the focus-mode inbox.
--
-- A digest collapses the last window of unread notifications into a
-- single email, sent at a user-chosen cadence (daily or weekly) and
-- hour-of-day. Pro-only.
--
-- One row per user (PRIMARY KEY user_id). A user can have at most
-- one digest schedule; the row exists iff the user has ever turned
-- digest on. `enabled=false` keeps the schedule shape but pauses
-- delivery without losing the user's preferences.
--
-- next_send_at is the hot column: the sweep query is
--   WHERE enabled AND next_send_at <= now()
-- which the partial index covers. After each delivery the worker
-- advances next_send_at by exactly one period (skipping missed
-- ticks the same way the cron workflow dispatcher does — the user
-- doesn't want a backlog of 17 digests after the server was offline).

-- +goose Up
CREATE TYPE user_notification_digest_frequency AS ENUM ('daily', 'weekly');

CREATE TABLE user_notification_digests (
    user_id      bigint    PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    enabled      boolean   NOT NULL DEFAULT false,
    frequency    user_notification_digest_frequency NOT NULL DEFAULT 'daily',
    hour_utc     smallint  NOT NULL DEFAULT 9,
    day_of_week  smallint,
    last_sent_at timestamptz,
    next_send_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_notification_digests_hour CHECK (hour_utc BETWEEN 0 AND 23),
    -- Weekly schedules carry a day_of_week (0=Sun..6=Sat to match
    -- Postgres DOW conventions); daily schedules leave it NULL.
    CONSTRAINT user_notification_digests_dow CHECK (
        (frequency = 'weekly' AND day_of_week BETWEEN 0 AND 6)
        OR (frequency = 'daily' AND day_of_week IS NULL)
    )
);

-- Hot path: the sweep claims due rows. Partial index keeps the tree
-- tiny for users without digest enabled — every Free user qualifies.
CREATE INDEX user_notification_digests_due_idx
    ON user_notification_digests (next_send_at)
    WHERE enabled AND next_send_at IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON user_notification_digests
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP INDEX IF EXISTS user_notification_digests_due_idx;
DROP TABLE IF EXISTS user_notification_digests;
DROP TYPE IF EXISTS user_notification_digest_frequency;
