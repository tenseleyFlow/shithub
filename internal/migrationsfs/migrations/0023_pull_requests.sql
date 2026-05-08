-- SPDX-License-Identifier: AGPL-3.0-or-later
--
-- S22 pull requests subsystem. PRs reuse the S21 issues table for
-- title/body/state/timeline/comments/labels/milestones/assignees;
-- this migration adds the PR-specific surface:
--
--   pull_requests          — keyed on issue_id; PR-only fields
--   pull_request_commits   — head-side commit list (refreshed on sync)
--   pull_request_files     — changed-files list (refreshed on sync)
--
-- Plus the merge-method config columns on `repos` that S11 deferred:
-- `allow_squash_merge`, `allow_rebase_merge`, `allow_merge_commit`,
-- `default_merge_method`. UI hides disallowed methods at merge time.

-- +goose Up

CREATE TYPE pr_merge_method AS ENUM ('merge', 'squash', 'rebase');
CREATE TYPE pr_mergeable_state AS ENUM ('unknown', 'clean', 'dirty', 'blocked', 'behind');
CREATE TYPE pr_file_status AS ENUM ('added', 'modified', 'deleted', 'renamed', 'copied');

ALTER TABLE repos
    ADD COLUMN allow_squash_merge   boolean         NOT NULL DEFAULT true,
    ADD COLUMN allow_rebase_merge   boolean         NOT NULL DEFAULT true,
    ADD COLUMN allow_merge_commit   boolean         NOT NULL DEFAULT true,
    ADD COLUMN default_merge_method pr_merge_method NOT NULL DEFAULT 'merge',
    ADD CONSTRAINT repos_at_least_one_merge_method CHECK (
        allow_squash_merge OR allow_rebase_merge OR allow_merge_commit
    );


CREATE TABLE pull_requests (
    issue_id              bigint              PRIMARY KEY REFERENCES issues(id) ON DELETE CASCADE,
    base_ref              text                NOT NULL,                       -- e.g. "trunk"
    head_ref              text                NOT NULL,                       -- e.g. "feature/x"
    head_repo_id          bigint              NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    -- Snapshotted at PR open + every synchronize so the diff renderer
    -- and merge-tree probe always have stable inputs even if the
    -- branches move between job ticks.
    base_oid              text                NOT NULL DEFAULT '',
    head_oid              text                NOT NULL DEFAULT '',
    draft                 boolean             NOT NULL DEFAULT false,
    mergeable             boolean,                                            -- NULL until first probe
    mergeable_state       pr_mergeable_state  NOT NULL DEFAULT 'unknown',
    merge_commit_sha      text,
    merged_at             timestamptz,
    merged_by_user_id     bigint              REFERENCES users(id) ON DELETE SET NULL,
    merge_method          pr_merge_method,
    base_oid_at_merge     text,
    head_oid_at_merge     text,
    last_synchronized_at  timestamptz,

    -- Same-repo PR can't merge a branch into itself. Cross-fork PRs
    -- (S27) will need a richer invariant covering head_repo_id != repo_id.
    CONSTRAINT pull_requests_base_head_distinct CHECK (base_ref <> head_ref)
);

CREATE INDEX pull_requests_head_idx ON pull_requests (head_repo_id, head_ref);
CREATE INDEX pull_requests_state_idx ON pull_requests (head_repo_id, mergeable_state);


CREATE TABLE pull_request_commits (
    pr_id            bigint      NOT NULL REFERENCES pull_requests(issue_id) ON DELETE CASCADE,
    sha              text        NOT NULL,
    position         int         NOT NULL,                                  -- 0-based, head-side order
    author_name      text        NOT NULL DEFAULT '',
    author_email     text        NOT NULL DEFAULT '',
    committer_name   text        NOT NULL DEFAULT '',
    committer_email  text        NOT NULL DEFAULT '',
    subject          text        NOT NULL DEFAULT '',
    body             text        NOT NULL DEFAULT '',
    authored_at      timestamptz,
    committed_at     timestamptz,

    PRIMARY KEY (pr_id, sha)
);

CREATE INDEX pull_request_commits_position_idx
    ON pull_request_commits (pr_id, position);


CREATE TABLE pull_request_files (
    pr_id      bigint          NOT NULL REFERENCES pull_requests(issue_id) ON DELETE CASCADE,
    path       text            NOT NULL,
    status     pr_file_status  NOT NULL,
    old_path   text,
    additions  int             NOT NULL DEFAULT 0,
    deletions  int             NOT NULL DEFAULT 0,
    changes    int             NOT NULL DEFAULT 0,

    PRIMARY KEY (pr_id, path)
);


-- +goose Down
DROP TABLE IF EXISTS pull_request_files;
DROP TABLE IF EXISTS pull_request_commits;
DROP TABLE IF EXISTS pull_requests;
ALTER TABLE repos
    DROP CONSTRAINT IF EXISTS repos_at_least_one_merge_method,
    DROP COLUMN IF EXISTS default_merge_method,
    DROP COLUMN IF EXISTS allow_merge_commit,
    DROP COLUMN IF EXISTS allow_rebase_merge,
    DROP COLUMN IF EXISTS allow_squash_merge;
DROP TYPE IF EXISTS pr_file_status;
DROP TYPE IF EXISTS pr_mergeable_state;
DROP TYPE IF EXISTS pr_merge_method;
