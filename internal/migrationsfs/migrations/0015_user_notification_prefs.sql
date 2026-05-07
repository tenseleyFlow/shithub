-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- Notification preferences as generic key/value rows. Schema is
-- intentionally light: a per-user (key, value) pair where value is JSONB
-- so future preferences (per-repo overrides, per-frequency settings,
-- digest schedules) don't need migrations.
--
-- Examples:
--   ('issues_email',           'true'::jsonb)
--   ('mentions_email',         'true'::jsonb)
--   ('pr_review_requests_email','true'::jsonb)
--   ('email_frequency',        '"per_event"'::jsonb)

-- +goose Up
CREATE TABLE user_notification_prefs (
    user_id    bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key        text        NOT NULL,
    value      jsonb       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON user_notification_prefs
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();

-- +goose Down
DROP TABLE IF EXISTS user_notification_prefs;
