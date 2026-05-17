-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-16a: rule-based notification routing for Pro users.
--
-- A rule is a (match, action) pair scoped to one user. When fanout
-- inserts an inbox row, it iterates the user's enabled rules in
-- `position` order and applies the first action that matches.
--
-- Match dimensions (any NULL = "no filter on this dimension"):
--   * match_reason   — exact match on notifications.reason
--                      ("mention", "review_requested", etc.)
--   * match_kind     — exact match on notifications.kind
--                      ("issue_comment_created", etc.)
--   * match_repo_id  — only events from this repo
--   * match_actor_id — only events whose actor is this user
--
-- Actions:
--   * snooze     — set snoozed_until = now() + action_snooze_minutes
--   * tab        — set tab_label = action_tab so the inbox can group
--   * mark_read  — flip unread=false on insert
--   * drop       — set tab_label='dropped' (the inbox hides this tab
--                  by default; we keep the row for audit so the user
--                  can still find the notification if they need to)
--
-- The mutation columns live on `notifications` (rather than a join
-- table) because every read path already selects from notifications;
-- a join would double the index pages touched per inbox view.

-- +goose Up
CREATE TYPE user_notification_rule_action AS ENUM (
    'snooze', 'tab', 'mark_read', 'drop'
);

CREATE TABLE user_notification_rules (
    id                    bigserial    PRIMARY KEY,
    user_id               bigint       NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name                  text         NOT NULL,
    enabled               boolean      NOT NULL DEFAULT true,
    position              integer      NOT NULL,

    -- match dimensions
    match_reason          text,
    match_kind            text,
    match_repo_id         bigint       REFERENCES repos(id) ON DELETE CASCADE,
    match_actor_id        bigint       REFERENCES users(id) ON DELETE SET NULL,

    -- action
    action                user_notification_rule_action NOT NULL,
    action_tab            text,
    action_snooze_minutes integer,

    created_at            timestamptz  NOT NULL DEFAULT now(),
    updated_at            timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT user_notification_rules_name_length CHECK (
        char_length(name) BETWEEN 1 AND 120
    ),
    CONSTRAINT user_notification_rules_position_nonneg CHECK (position >= 0),
    CONSTRAINT user_notification_rules_action_tab_length CHECK (
        action_tab IS NULL OR char_length(action_tab) BETWEEN 1 AND 64
    ),
    CONSTRAINT user_notification_rules_match_reason_length CHECK (
        match_reason IS NULL OR char_length(match_reason) BETWEEN 1 AND 32
    ),
    CONSTRAINT user_notification_rules_match_kind_length CHECK (
        match_kind IS NULL OR char_length(match_kind) BETWEEN 1 AND 64
    ),
    -- Snooze must carry positive minutes; tab must carry a label; the
    -- other two actions must not. Enforce at the DB so a malformed
    -- INSERT can't bypass handler validation.
    CONSTRAINT user_notification_rules_action_params CHECK (
        (action = 'snooze' AND action_snooze_minutes IS NOT NULL AND action_snooze_minutes > 0 AND action_tab IS NULL)
        OR (action = 'tab' AND action_tab IS NOT NULL AND action_snooze_minutes IS NULL)
        OR (action IN ('mark_read', 'drop') AND action_tab IS NULL AND action_snooze_minutes IS NULL)
    ),
    -- Evaluation order is per-user; collisions would make the order
    -- ambiguous, so reject them up front. Handlers always allocate a
    -- fresh max(position)+1 for inserts.
    UNIQUE (user_id, position)
);

-- Hot path: load a user's enabled rules in evaluation order. Partial
-- index keeps the tree tiny for users with many disabled rules.
CREATE INDEX user_notification_rules_user_enabled_idx
    ON user_notification_rules (user_id, position) WHERE enabled;

CREATE TRIGGER set_updated_at BEFORE UPDATE ON user_notification_rules
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- ─── notification mutation columns ────────────────────────────────
--
-- Three nullable additions to `notifications` so the fanout-side
-- rule applier can stamp the routing decision onto the row that
-- triggered it. All three default NULL so existing rows are
-- semantically unchanged: snoozed_until=NULL → not snoozed,
-- tab_label=NULL → default "Inbox" bucket, matched_rule_id=NULL →
-- no rule fired (read paths can show this as "no rule matched").

ALTER TABLE notifications
    ADD COLUMN snoozed_until    timestamptz,
    ADD COLUMN tab_label        text,
    ADD COLUMN matched_rule_id  bigint REFERENCES user_notification_rules(id) ON DELETE SET NULL;

-- Inbox-by-tab lookups (e.g. "show me the security_alerts tab").
-- Partial keeps the index small — most rows land in default Inbox.
CREATE INDEX notifications_tab_label_idx
    ON notifications (recipient_user_id, tab_label, last_event_at DESC)
    WHERE tab_label IS NOT NULL;

-- Snoozed-still-quiet check on inbox read. Recipient + a not-yet-
-- expired snooze is the common shape; sort by the wake-up timestamp
-- so a future "show me what wakes up soonest" view is cheap.
CREATE INDEX notifications_snoozed_idx
    ON notifications (recipient_user_id, snoozed_until)
    WHERE snoozed_until IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS notifications_snoozed_idx;
DROP INDEX IF EXISTS notifications_tab_label_idx;
ALTER TABLE notifications
    DROP COLUMN IF EXISTS matched_rule_id,
    DROP COLUMN IF EXISTS tab_label,
    DROP COLUMN IF EXISTS snoozed_until;
DROP INDEX IF EXISTS user_notification_rules_user_enabled_idx;
DROP TABLE IF EXISTS user_notification_rules;
DROP TYPE IF EXISTS user_notification_rule_action;
