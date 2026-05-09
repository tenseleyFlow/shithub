-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S29 notifications inbox + per-thread subscription + email send log.
--
-- `domain_events` was already shipped in S26 (per the S00–S25
-- audit's forward-deferral). The fan-out worker this sprint lands
-- consumes from there.
--
-- Schema decisions:
--
-- * `notifications` is the user-facing inbox. One row per
--   (recipient, thread). Multiple events on the same thread coalesce
--   into the same row (last_event_at + last_actor_user_id update).
--
-- * `notification_threads` is the per-user explicit subscription
--   override on top of the per-repo `watches` rule. A row here lets
--   the user pin themselves to a thread (or ignore it) regardless
--   of the repo-level watch.
--
-- * `notification_email_log` is for de-dup, abuse triage, and the
--   storm dampener's per-thread cap.
--
-- * `domain_events_processed` records the fan-out worker's cursor
--   so a restart resumes where it stopped, and so the worker can
--   detect drift if a row is somehow consumed twice. Keyed
--   (consumer, last_event_id) so S33 webhooks shares the table
--   shape with its own consumer name later.

-- +goose Up

CREATE TYPE notification_thread_kind AS ENUM ('issue', 'pr');

CREATE TABLE notifications (
    id                  bigserial   PRIMARY KEY,
    recipient_user_id   bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind                text        NOT NULL,
    reason              text        NOT NULL,
    repo_id             bigint      REFERENCES repos(id) ON DELETE CASCADE,
    thread_kind         notification_thread_kind,
    thread_id           bigint,
    -- source_event_id points at the most recent domain_events row
    -- this notification was last touched by. Useful for debugging
    -- + for the read-state UI ("which event did I last see?").
    source_event_id     bigint,
    unread              boolean     NOT NULL DEFAULT true,
    last_event_at       timestamptz NOT NULL DEFAULT now(),
    last_actor_user_id  bigint      REFERENCES users(id) ON DELETE SET NULL,
    summary             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT notifications_kind_length CHECK (char_length(kind) BETWEEN 1 AND 64),
    CONSTRAINT notifications_reason_length CHECK (char_length(reason) BETWEEN 1 AND 32)
);

-- Inbox view: "all my notifications, recent first."
CREATE INDEX notifications_recipient_recent_idx
    ON notifications (recipient_user_id, last_event_at DESC);

-- Unread badge count: filtered partial index keeps the count cheap
-- even when the recipient has tens of thousands of read rows.
CREATE INDEX notifications_recipient_unread_idx
    ON notifications (recipient_user_id)
    WHERE unread = true;

-- Coalesce lookup: "do I already have an inbox row for this thread?"
CREATE UNIQUE INDEX notifications_thread_coalesce_idx
    ON notifications (recipient_user_id, thread_kind, thread_id)
    WHERE thread_id IS NOT NULL;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON notifications
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- ─── per-thread subscription ────────────────────────────────────────

CREATE TABLE notification_threads (
    recipient_user_id bigint                      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    thread_kind       notification_thread_kind    NOT NULL,
    thread_id         bigint                      NOT NULL,
    subscribed        boolean                     NOT NULL,
    reason            text                        NOT NULL,
    updated_at        timestamptz                 NOT NULL DEFAULT now(),
    PRIMARY KEY (recipient_user_id, thread_kind, thread_id)
);

CREATE INDEX notification_threads_thread_idx
    ON notification_threads (thread_kind, thread_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON notification_threads
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- ─── email send log (storm dampener + abuse triage) ─────────────────

CREATE TABLE notification_email_log (
    id                bigserial   PRIMARY KEY,
    recipient_user_id bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    notification_id   bigint      REFERENCES notifications(id) ON DELETE SET NULL,
    thread_kind       notification_thread_kind,
    thread_id         bigint,
    sent_at           timestamptz NOT NULL DEFAULT now(),
    message_id        text
);

-- Storm dampener: "did I email this recipient about this thread in
-- the last N minutes?" The partial-index shape isn't great because
-- the predicate is time-relative; just key on the lookup columns
-- and let the planner narrow.
CREATE INDEX notification_email_log_recent_idx
    ON notification_email_log (recipient_user_id, thread_kind, thread_id, sent_at DESC);

-- Per-recipient absolute rate cap (e.g. "≤ 100 emails per hour"):
-- recency-sorted by recipient.
CREATE INDEX notification_email_log_recipient_idx
    ON notification_email_log (recipient_user_id, sent_at DESC);

-- ─── fan-out cursor ────────────────────────────────────────────────

CREATE TABLE domain_events_processed (
    consumer        text        NOT NULL,
    last_event_id   bigint      NOT NULL,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer)
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON domain_events_processed
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- Seed the notify cursor at 0 so a fresh DB doesn't trip the
-- "first run" branch in the worker (which would otherwise have
-- to handle the absent-row case).
INSERT INTO domain_events_processed (consumer, last_event_id)
VALUES ('notify_fanout', 0);

-- +goose Down
DROP TABLE IF EXISTS domain_events_processed;
DROP TABLE IF EXISTS notification_email_log;
DROP TABLE IF EXISTS notification_threads;
DROP TABLE IF EXISTS notifications;
DROP TYPE IF EXISTS notification_thread_kind;
