-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S23 PR-review subsystem.
--
--   pr_reviews          — one row per submitted review (comment / approve /
--                         request_changes); also dismissable with reason.
--   pr_review_comments  — file-level inline comments anchored on
--                         (file_path, side, original_commit_sha,
--                         original_line, original_position). Each is
--                         either part of a submitted review (review_id
--                         non-NULL) or a single inline comment outside a
--                         review (review_id IS NULL, pending=false), or a
--                         server-side draft that hasn't been submitted yet
--                         (review_id IS NULL, pending=true).
--   pr_review_requests  — pending review requests; satisfied when the
--                         requested user submits an approve/request_changes.
--
-- Branch protection extensions (the actual gate evaluation lives in
-- internal/pulls/review/required.go):
--
--   required_review_count           int  DEFAULT 0
--   dismiss_stale_reviews_on_push   bool DEFAULT false
--   require_code_owner_review       bool DEFAULT false  (placeholder; CODEOWNERS post-MVP)

-- +goose Up

CREATE TYPE pr_review_state AS ENUM ('comment', 'approve', 'request_changes');
CREATE TYPE pr_review_side  AS ENUM ('left', 'right');

CREATE TABLE pr_reviews (
    id                    bigserial         PRIMARY KEY,
    pr_issue_id           bigint            NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    author_user_id        bigint            REFERENCES users(id) ON DELETE SET NULL,
    state                 pr_review_state   NOT NULL,
    body                  text              NOT NULL DEFAULT '',
    body_html_cached      text,
    submitted_at          timestamptz       NOT NULL DEFAULT now(),
    dismissed_at          timestamptz,
    dismissed_by_user_id  bigint            REFERENCES users(id) ON DELETE SET NULL,
    dismissal_reason      text              NOT NULL DEFAULT ''
);

CREATE INDEX pr_reviews_pr_idx
    ON pr_reviews (pr_issue_id, submitted_at DESC);
CREATE INDEX pr_reviews_state_idx
    ON pr_reviews (pr_issue_id, state) WHERE dismissed_at IS NULL;


CREATE TABLE pr_review_comments (
    id                    bigserial         PRIMARY KEY,
    pr_issue_id           bigint            NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    review_id             bigint            REFERENCES pr_reviews(id) ON DELETE SET NULL,
    author_user_id        bigint            REFERENCES users(id) ON DELETE SET NULL,
    file_path             text              NOT NULL,
    side                  pr_review_side    NOT NULL DEFAULT 'right',
    original_commit_sha   text              NOT NULL,
    original_line         int               NOT NULL,
    original_position     int               NOT NULL,
    -- NULL means the comment has gone outdated (no equivalent line on the
    -- current head). Comments still display in the timeline + a "Show
    -- outdated" toggle in the Files tab.
    current_position      int,
    body                  text              NOT NULL,
    body_html_cached      text,
    in_reply_to_id        bigint            REFERENCES pr_review_comments(id) ON DELETE SET NULL,
    -- Server-side draft state: pending=true rows belong to a draft review
    -- the author hasn't submitted yet. They flip to pending=false +
    -- review_id=N when the review is submitted.
    pending               boolean           NOT NULL DEFAULT false,
    resolved_at           timestamptz,
    resolved_by_user_id   bigint            REFERENCES users(id) ON DELETE SET NULL,
    created_at            timestamptz       NOT NULL DEFAULT now(),
    updated_at            timestamptz       NOT NULL DEFAULT now(),
    edited_at             timestamptz,

    CONSTRAINT pr_review_comments_body_length CHECK (char_length(body) BETWEEN 1 AND 65535)
);

CREATE INDEX pr_review_comments_pr_idx
    ON pr_review_comments (pr_issue_id, created_at);
CREATE INDEX pr_review_comments_review_idx
    ON pr_review_comments (review_id) WHERE review_id IS NOT NULL;
CREATE INDEX pr_review_comments_drafts_idx
    ON pr_review_comments (pr_issue_id, author_user_id) WHERE pending = true;
CREATE INDEX pr_review_comments_threads_idx
    ON pr_review_comments (in_reply_to_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON pr_review_comments
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


CREATE TABLE pr_review_requests (
    id                       bigserial    PRIMARY KEY,
    pr_issue_id              bigint       NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    requested_user_id        bigint       REFERENCES users(id) ON DELETE CASCADE,
    -- Teams arrive in S31; column is in from day one so the migration
    -- doesn't need to touch this table when teams ship.
    requested_team_id        bigint,
    requested_by_user_id     bigint       REFERENCES users(id) ON DELETE SET NULL,
    requested_at             timestamptz  NOT NULL DEFAULT now(),
    dismissed_at             timestamptz,
    satisfied_by_review_id   bigint       REFERENCES pr_reviews(id) ON DELETE SET NULL,

    CONSTRAINT pr_review_requests_target_xor CHECK (
        (requested_user_id IS NOT NULL) <> (requested_team_id IS NOT NULL)
    )
);

CREATE INDEX pr_review_requests_user_pending_idx
    ON pr_review_requests (requested_user_id)
    WHERE dismissed_at IS NULL AND satisfied_by_review_id IS NULL;
CREATE INDEX pr_review_requests_pr_idx
    ON pr_review_requests (pr_issue_id);


-- Branch-protection knobs.
ALTER TABLE branch_protection_rules
    ADD COLUMN required_review_count          int  NOT NULL DEFAULT 0,
    ADD COLUMN dismiss_stale_reviews_on_push  bool NOT NULL DEFAULT false,
    ADD COLUMN require_code_owner_review      bool NOT NULL DEFAULT false,
    ADD CONSTRAINT branch_protection_rules_required_review_nonneg CHECK (required_review_count >= 0);


-- +goose Down
ALTER TABLE branch_protection_rules
    DROP CONSTRAINT IF EXISTS branch_protection_rules_required_review_nonneg,
    DROP COLUMN IF EXISTS require_code_owner_review,
    DROP COLUMN IF EXISTS dismiss_stale_reviews_on_push,
    DROP COLUMN IF EXISTS required_review_count;
DROP TABLE IF EXISTS pr_review_requests;
DROP TABLE IF EXISTS pr_review_comments;
DROP TABLE IF EXISTS pr_reviews;
DROP TYPE IF EXISTS pr_review_side;
DROP TYPE IF EXISTS pr_review_state;
