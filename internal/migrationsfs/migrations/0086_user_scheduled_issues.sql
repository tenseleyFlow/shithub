-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- PRO-EXT01-07b: scheduled issues. A Pro user can prepare an issue
-- and pick a future datetime; a worker creates the real row at that
-- time. The schedule row stays around (status=created) after the
-- worker lands the issue so the user can audit what fired.
--
-- The worker uses the existing jobs.run_at lane: each schedule row
-- enqueues a jobs row whose payload references this id, so we get
-- atomic retries + at-least-once delivery without a separate timer.
-- The worker handler joins back to this table and reads
-- (title, body, repo_id, user_id, status) so an admin can cancel a
-- queued schedule by setting status=cancelled and have the worker
-- short-circuit at job time.

-- +goose Up
CREATE TYPE scheduled_issue_status AS ENUM ('pending', 'cancelled', 'created', 'failed');

CREATE TABLE user_scheduled_issues (
    id              bigserial   PRIMARY KEY,
    user_id         bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    repo_id         bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    title           text        NOT NULL,
    body            text        NOT NULL,
    schedule_at     timestamptz NOT NULL,
    status          scheduled_issue_status NOT NULL DEFAULT 'pending',
    created_issue_id bigint     NULL REFERENCES issues(id) ON DELETE SET NULL,
    failure_reason  text        NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT user_scheduled_issues_title_shape CHECK (
        char_length(title) BETWEEN 1 AND 256
    ),
    CONSTRAINT user_scheduled_issues_body_shape CHECK (
        char_length(body) BETWEEN 0 AND 65535
    )
);

-- Per-user listing in the settings view: pending-first, then newest
-- created/failed/cancelled. Partial index makes the hot-path settings
-- list scan tight.
CREATE INDEX user_scheduled_issues_user_pending_idx
    ON user_scheduled_issues (user_id, schedule_at)
    WHERE status = 'pending';

CREATE INDEX user_scheduled_issues_user_id_idx
    ON user_scheduled_issues (user_id, schedule_at DESC);

-- +goose Down
DROP TABLE IF EXISTS user_scheduled_issues;
DROP TYPE IF EXISTS scheduled_issue_status;
