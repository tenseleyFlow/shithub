-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S21 issues subsystem. Eight tables plus the per-repo counter:
--
--   repo_issue_counter   — per-repo monotonic numbering for issues+PRs
--   issues               — the row; `kind` discriminator covers PRs (S22)
--   issue_comments       — per-issue threaded comments
--   issue_assignees      — many-to-many issue ↔ user
--   labels               — repo-scoped labels (default set seeded on create)
--   issue_labels         — many-to-many issue ↔ label
--   milestones           — repo-scoped milestones
--   issue_events         — generic polymorphic timeline (label/assign/etc.)
--   issue_references     — cross-reference index (comment/body/commit → issue)
--
-- The `kind` discriminator on `issues` ('issue'|'pr') is in from day 1
-- so PR rows in S22 are first-class and don't require a schema split.
-- PR-specific fields live in a `pull_requests` table keyed on issue_id
-- (built in S22).

-- +goose Up

CREATE TYPE issue_kind         AS ENUM ('issue', 'pr');
CREATE TYPE issue_state        AS ENUM ('open', 'closed');
CREATE TYPE issue_state_reason AS ENUM ('completed', 'not_planned', 'reopened', 'duplicate');
CREATE TYPE milestone_state    AS ENUM ('open', 'closed');
CREATE TYPE issue_ref_source   AS ENUM ('comment_body', 'issue_body', 'commit_message');

-- Per-repo monotonic counter. Allocation is one row UPDATE inside the
-- creating transaction so concurrent inserts can't collide. Issues +
-- PRs share the counter (matches GitHub's #N space).
CREATE TABLE repo_issue_counter (
    repo_id     bigint  PRIMARY KEY REFERENCES repos(id) ON DELETE CASCADE,
    next_number bigint  NOT NULL DEFAULT 1
);

CREATE TABLE issues (
    id                  bigserial         PRIMARY KEY,
    repo_id             bigint            NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    number              bigint            NOT NULL,
    kind                issue_kind        NOT NULL DEFAULT 'issue',
    title               text              NOT NULL,
    body                text              NOT NULL DEFAULT '',
    body_html_cached    text,
    md_pipeline_version int               NOT NULL DEFAULT 1,
    author_user_id      bigint            REFERENCES users(id) ON DELETE SET NULL,
    state               issue_state       NOT NULL DEFAULT 'open',
    state_reason        issue_state_reason,
    locked              boolean           NOT NULL DEFAULT false,
    lock_reason         text,
    milestone_id        bigint, -- FK added below after milestones exists
    created_at          timestamptz       NOT NULL DEFAULT now(),
    updated_at          timestamptz       NOT NULL DEFAULT now(),
    edited_at           timestamptz,
    closed_at           timestamptz,
    closed_by_user_id   bigint            REFERENCES users(id) ON DELETE SET NULL,

    CONSTRAINT issues_title_length CHECK (char_length(title) BETWEEN 1 AND 256),
    CONSTRAINT issues_body_length  CHECK (char_length(body)  <= 65535),

    UNIQUE (repo_id, number)
);

CREATE INDEX issues_repo_state_updated_idx
    ON issues (repo_id, state, updated_at DESC);
CREATE INDEX issues_repo_kind_state_idx
    ON issues (repo_id, kind, state);
CREATE INDEX issues_author_idx ON issues (author_user_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON issues
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


CREATE TABLE issue_comments (
    id                  bigserial   PRIMARY KEY,
    issue_id            bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    author_user_id      bigint      REFERENCES users(id) ON DELETE SET NULL,
    body                text        NOT NULL,
    body_html_cached    text,
    md_pipeline_version int         NOT NULL DEFAULT 1,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    edited_at           timestamptz,

    CONSTRAINT issue_comments_body_length CHECK (char_length(body) BETWEEN 1 AND 65535)
);

CREATE INDEX issue_comments_issue_idx ON issue_comments (issue_id, created_at);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON issue_comments
    FOR EACH ROW EXECUTE FUNCTION tg_set_updated_at();


CREATE TABLE issue_assignees (
    issue_id            bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    user_id             bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    assigned_at         timestamptz NOT NULL DEFAULT now(),
    assigned_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,

    PRIMARY KEY (issue_id, user_id)
);

CREATE INDEX issue_assignees_user_idx ON issue_assignees (user_id);


CREATE TABLE labels (
    id          bigserial   PRIMARY KEY,
    repo_id     bigint      NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    name        citext      NOT NULL,
    color       text        NOT NULL DEFAULT 'd0d7de',
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT labels_name_length CHECK (char_length(name::text) BETWEEN 1 AND 50),
    CONSTRAINT labels_color_format CHECK (color ~ '^[0-9a-fA-F]{6}$'),

    UNIQUE (repo_id, name)
);


CREATE TABLE issue_labels (
    issue_id           bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    label_id           bigint      NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    applied_at         timestamptz NOT NULL DEFAULT now(),
    applied_by_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,

    PRIMARY KEY (issue_id, label_id)
);

CREATE INDEX issue_labels_label_idx ON issue_labels (label_id);


CREATE TABLE milestones (
    id          bigserial         PRIMARY KEY,
    repo_id     bigint            NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    title       text              NOT NULL,
    description text              NOT NULL DEFAULT '',
    state       milestone_state   NOT NULL DEFAULT 'open',
    due_on      timestamptz,
    created_at  timestamptz       NOT NULL DEFAULT now(),
    closed_at   timestamptz,

    CONSTRAINT milestones_title_length CHECK (char_length(title) BETWEEN 1 AND 200),

    UNIQUE (repo_id, title)
);

ALTER TABLE issues ADD CONSTRAINT issues_milestone_fk
    FOREIGN KEY (milestone_id) REFERENCES milestones(id) ON DELETE SET NULL;


CREATE TABLE issue_events (
    id            bigserial   PRIMARY KEY,
    issue_id      bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    actor_user_id bigint      REFERENCES users(id) ON DELETE SET NULL,
    kind          text        NOT NULL,
    meta          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    ref_target_id bigint, -- denormalized when the event references another issue
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX issue_events_issue_idx ON issue_events (issue_id, created_at);


CREATE TABLE issue_references (
    id                bigserial   PRIMARY KEY,
    source_issue_id   bigint      REFERENCES issues(id) ON DELETE SET NULL,
    target_issue_id   bigint      NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    source_kind       issue_ref_source NOT NULL,
    source_object_id  bigint, -- comment_id when source_kind=comment_body; issue_id when issue_body; push_event_id when commit_message
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX issue_references_target_idx ON issue_references (target_issue_id, created_at);
CREATE INDEX issue_references_source_idx ON issue_references (source_issue_id, source_kind);


-- +goose Down
DROP TABLE IF EXISTS issue_references;
DROP TABLE IF EXISTS issue_events;
ALTER TABLE issues DROP CONSTRAINT IF EXISTS issues_milestone_fk;
DROP TABLE IF EXISTS milestones;
DROP TABLE IF EXISTS issue_labels;
DROP TABLE IF EXISTS labels;
DROP TABLE IF EXISTS issue_assignees;
DROP TABLE IF EXISTS issue_comments;
DROP TABLE IF EXISTS issues;
DROP TABLE IF EXISTS repo_issue_counter;
DROP TYPE IF EXISTS issue_ref_source;
DROP TYPE IF EXISTS milestone_state;
DROP TYPE IF EXISTS issue_state_reason;
DROP TYPE IF EXISTS issue_state;
DROP TYPE IF EXISTS issue_kind;
